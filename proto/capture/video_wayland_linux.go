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

	// 5) gst: pipewiresrc(fd 3) → H264 Annex-B в stdout
	pipe := gstPipeline(opts, node)
	cmd := exec.CommandContext(ctx, gst, pipe...)
	cmd.ExtraFiles = []*os.File{pwFile} // становится fd 3 у gst
	log.Printf("capture: gst video (wayland) node=%d %s", node, strings.Join(pipe, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		pwFile.Close()
		conn.Close()
		return nil, fmt.Errorf("start gst: %w", err)
	}
	pwFile.Close() // копию fd держит gst

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer conn.Close()
		defer waitKill(cmd, "video")
		readH264(ctx, bufio.NewReader(stdout), frames, opts.DropLate)
	}()
	return frames, nil
}

// gstHas — есть ли gst-элемент (проверяем через gst-inspect-1.0).
func gstHas(el string) bool {
	p, err := exec.LookPath("gst-inspect-1.0")
	if err != nil {
		return false
	}
	return exec.Command(p, el).Run() == nil
}

// gstEncoderChain — H264-кодер, доступный в этой сборке GStreamer, с параметрами.
// Приоритет: аппаратный VAAPI (на AMD Deck — почти бесплатно по CPU, тянет 4K) →
// софтовые openh264/x264. У разных кодеров разные свойства и единицы битрейта.
func gstEncoderChain(kbps, fps int) string {
	switch {
	case gstHas("vah264enc"): // GStreamer VA (gst-plugins-bad), аппаратный
		return fmt.Sprintf("vapostproc ! vah264enc bitrate=%d key-int-max=%d", kbps, fps)
	case gstHas("vaapih264enc"): // gstreamer-vaapi, аппаратный
		return fmt.Sprintf("vaapipostproc ! vaapih264enc rate-control=cbr bitrate=%d keyframe-period=%d", kbps, fps)
	case gstHas("openh264enc"): // Cisco OpenH264, софтовый (bitrate в БИТ/с)
		return fmt.Sprintf("openh264enc bitrate=%d gop-size=%d complexity=0", kbps*1000, fps)
	default: // x264enc, софтовый
		return fmt.Sprintf("x264enc tune=zerolatency speed-preset=ultrafast bitrate=%d key-int-max=%d", kbps, fps)
	}
}

// gstPipeline строит описание gst-пайплайна: PipeWire → (скейл) → H264 (доступный
// кодер) → Annex-B H264 в stdout. h264parse config-interval=1 повторяет SPS/PPS
// (раз в секунду), чтобы новый зритель декодировал.
func gstPipeline(opts Options, node uint32) []string {
	kbps := bitrateKbps(opts.Bitrate)
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	rawCaps := fmt.Sprintf("video/x-raw,format=I420,framerate=%d/1", fps)
	scale := ""
	if opts.Width > 0 {
		if sw, sh := ScreenSize(); sw > 0 && sh > 0 {
			th := opts.Width * sh / sw
			th -= th % 2
			rawCaps = fmt.Sprintf("video/x-raw,format=I420,framerate=%d/1,width=%d,height=%d", fps, opts.Width, th)
		}
		scale = "videoscale ! "
	}
	desc := fmt.Sprintf(
		"pipewiresrc fd=3 path=%d do-timestamp=true keepalive-time=1000 ! "+
			"videoconvert ! videorate ! %s%s ! "+
			"%s ! "+
			"h264parse config-interval=1 ! "+
			"video/x-h264,stream-format=byte-stream,alignment=au ! "+
			"fdsink fd=1 sync=false",
		node, scale, rawCaps, gstEncoderChain(kbps, fps))
	return []string{"-q", desc}
}
