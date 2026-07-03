//go:build linux

package capture

// Видео на Wayland через xdg-desktop-portal ScreenCast (то же, чем снимают экран
// OBS/Chrome). Хореография по D-Bus: CreateSession → SelectSources → Start →
// OpenPipeWireRemote (даёт PipeWire-fd + node id). Кадры из PipeWire кодируем
// GStreamer'ом (pipewiresrc → x264enc → Annex-B H264 в stdout), дальше — общий
// readH264. Портал спрашивает у пользователя разрешение (один раз диалог KDE).
//
// Требует: xdg-desktop-portal(-kde), gst-launch-1.0 с плагинами pipewire и x264.
// На Wayland сейчас поддержан H264 (x264enc); VP8-путь через gst не сделан.

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

var (
	tokMu  sync.Mutex
	tokSeq uint64
)

func newHandleToken() string {
	tokMu.Lock()
	tokSeq++
	n := tokSeq
	tokMu.Unlock()
	return fmt.Sprintf("katana%d", n)
}

// portalRequest вызывает метод портала, возвращающий Request-объект, и ждёт его
// сигнал Response (портал асинхронный: результат приходит сигналом, а не в ответе
// вызова). opts дополняется handle_token; args — позиционные аргументы метода.
func portalRequest(conn *dbus.Conn, obj dbus.BusObject, method string, opts map[string]dbus.Variant, args ...interface{}) (map[string]dbus.Variant, error) {
	token := newHandleToken()
	opts["handle_token"] = dbus.MakeVariant(token)
	unique := strings.ReplaceAll(strings.TrimPrefix(conn.Names()[0], ":"), ".", "_")
	reqPath := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + unique + "/" + token)

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(reqPath),
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, err
	}
	ch := make(chan *dbus.Signal, 8)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	callArgs := append(args, opts)
	var reqObjPath dbus.ObjectPath
	if err := obj.Call(method, 0, callArgs...).Store(&reqObjPath); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	for {
		select {
		case sig := <-ch:
			if sig.Path != reqPath || sig.Name != "org.freedesktop.portal.Request.Response" {
				continue
			}
			var code uint32
			var results map[string]dbus.Variant
			if err := dbus.Store(sig.Body, &code, &results); err != nil {
				return nil, fmt.Errorf("%s: decode response: %w", method, err)
			}
			if code != 0 {
				return nil, fmt.Errorf("%s: response code %d (отказ/отмена пользователем?)", method, code)
			}
			return results, nil
		case <-time.After(120 * time.Second):
			return nil, fmt.Errorf("%s: таймаут портала (нет ответа пользователя)", method)
		}
	}
}

func sessionHandleOf(res map[string]dbus.Variant) string {
	switch v := res["session_handle"].Value().(type) {
	case string:
		return v
	case dbus.ObjectPath:
		return string(v)
	}
	return ""
}

// startVideoWayland поднимает портал ScreenCast + gst и возвращает канал кадров.
func startVideoWayland(ctx context.Context, opts Options) (chan []byte, error) {
	gst := gstLaunchPath()
	if gst == "" {
		return nil, fmt.Errorf("gst-launch-1.0 не найден (нужен для Wayland-захвата)")
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	obj := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	// 1) CreateSession
	res, err := portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.CreateSession",
		map[string]dbus.Variant{"session_handle_token": dbus.MakeVariant(newHandleToken())})
	if err != nil {
		conn.Close()
		return nil, err
	}
	sh := sessionHandleOf(res)
	if sh == "" {
		conn.Close()
		return nil, fmt.Errorf("портал не вернул session_handle")
	}
	session := dbus.ObjectPath(sh)

	// 2) SelectSources: монитор целиком, курсор в кадре
	if _, err := portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.SelectSources",
		map[string]dbus.Variant{
			"types":       dbus.MakeVariant(uint32(1)), // MONITOR
			"cursor_mode": dbus.MakeVariant(uint32(2)), // EMBEDDED — курсор в видео
			"multiple":    dbus.MakeVariant(false),
		}, session); err != nil {
		conn.Close()
		return nil, err
	}

	// 3) Start → streams (node id PipeWire). Тут KDE показывает диалог разрешения.
	res, err = portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.Start",
		map[string]dbus.Variant{}, session, "")
	if err != nil {
		conn.Close()
		return nil, err
	}
	var streams []struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	if err := dbus.Store([]interface{}{res["streams"].Value()}, &streams); err != nil || len(streams) == 0 {
		conn.Close()
		return nil, fmt.Errorf("нет ScreenCast-потоков: %v", err)
	}
	node := streams[0].Node

	// 4) OpenPipeWireRemote → fd (обычный вызов, fd в ответе)
	var fd dbus.UnixFD
	if err := obj.Call("org.freedesktop.portal.ScreenCast.OpenPipeWireRemote", 0,
		session, map[string]dbus.Variant{}).Store(&fd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("OpenPipeWireRemote: %w", err)
	}
	pwFile := os.NewFile(uintptr(fd), "pipewire")

	// 5) gst забирает кадры из PipeWire и отдаёт СЫРОЙ I420 (фикс. WxH) в stdout;
	// кодирует их ffmpeg (libx264/libvpx — есть в ffmpeg, в отличие от gst x264enc).
	// Так мы не зависим от кодер-плагинов GStreamer.
	w, h := waylandSize(opts)
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}

	gstArgs := gstRawPipeline(node, w, h, fps)
	gstCmd := exec.CommandContext(ctx, gst, gstArgs...)
	gstCmd.ExtraFiles = []*os.File{pwFile} // fd 3 у gst
	log.Printf("capture: gst grab (wayland) node=%d %dx%d %s", node, w, h, strings.Join(gstArgs, " "))

	ffArgs := waylandEncodeArgs(opts, w, h, fps)
	ffCmd := exec.CommandContext(ctx, FFmpegPath(), ffArgs...)
	log.Printf("capture: ffmpeg encode (wayland) %s", strings.Join(ffArgs, " "))

	// Связь gst.stdout → ffmpeg.stdin напрямую (kernel pipe, без копирования).
	rp, wp, perr := os.Pipe()
	if perr != nil {
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("pipe: %w", perr)
	}
	gstCmd.Stdout = wp
	ffCmd.Stdin = rp
	gstErr, _ := gstCmd.StderrPipe()
	go logStderr(gstErr)
	ffErr, _ := ffCmd.StderrPipe()
	go logStderr(ffErr)
	ffOut, err := ffCmd.StdoutPipe()
	if err != nil {
		rp.Close()
		wp.Close()
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}

	if err := gstCmd.Start(); err != nil {
		rp.Close()
		wp.Close()
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("start gst: %w", err)
	}
	if err := ffCmd.Start(); err != nil {
		rp.Close()
		wp.Close()
		pwFile.Close()
		_ = gstCmd.Process.Kill()
		_ = gstCmd.Wait()
		conn.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	// Концы пайпа и fd PipeWire держат уже дочерние процессы — в родителе закрываем.
	rp.Close()
	wp.Close()
	pwFile.Close()

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer conn.Close()
		defer waitKill(gstCmd, "video-grab")
		defer waitKill(ffCmd, "video-encode")
		in := bufio.NewReader(ffOut)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()
	return frames, nil
}

func even(v int) int { return v - v%2 }

// waylandSize — целевой размер сырого кадра: даунскейл до opts.Width (аспект по
// экрану), либо нативное разрешение экрана, либо дефолт.
func waylandSize(opts Options) (int, int) {
	if opts.Width > 0 {
		if sw, sh := ScreenSize(); sw > 0 && sh > 0 {
			return even(opts.Width), even(opts.Width * sh / sw)
		}
		return even(opts.Width), even(opts.Width * 9 / 16)
	}
	if sw, sh := ScreenSize(); sw > 0 && sh > 0 {
		return even(sw), even(sh)
	}
	return 1920, 1080
}

// gstRawPipeline — gst забирает PipeWire, приводит к I420 фиксированного WxH и
// пишет сырьё в stdout (fdsink fd=1). Никакого кодирования в gst.
func gstRawPipeline(node uint32, w, h, fps int) []string {
	desc := fmt.Sprintf(
		"pipewiresrc fd=3 path=%d do-timestamp=true keepalive-time=1000 ! "+
			"videoconvert ! videorate ! videoscale ! "+
			"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1 ! "+
			"fdsink fd=1 sync=false",
		node, w, h, fps)
	// Пайплайн — ОТДЕЛЬНЫМИ токенами (как в шелле): единый аргумент с пробелами
	// gst-launch трактует как одно имя элемента → syntax error. -q гасит прогресс
	// в stdout (туда fdsink пишет сырьё).
	return append([]string{"-q"}, strings.Fields(desc)...)
}

// waylandEncodeArgs — ffmpeg читает сырой I420 (WxH) из stdin и кодирует в
// H264 (libx264) или VP8 (libvpx), как в X11-пути.
func waylandEncodeArgs(opts Options, w, h, fps int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "rawvideo", "-pixel_format", "yuv420p",
		"-video_size", fmt.Sprintf("%dx%d", w, h),
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", "-",
		"-r", fmt.Sprintf("%d", fps), "-fps_mode", "cfr", "-pix_fmt", "yuv420p",
	}
	if opts.Codec == CodecH264 {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "high", "-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", fps),
			"-bsf:v", "dump_extra=freq=keyframe",
			"-f", "h264", "pipe:1")
	} else {
		args = append(args,
			"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8",
			"-lag-in-frames", "0", "-threads", fmt.Sprintf("%d", opts.Threads),
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", fps),
			"-keyint_min", fmt.Sprintf("%d", fps),
			"-f", "ivf", "pipe:1")
	}
	return args
}
