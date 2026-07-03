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

// startVideoWaylandGst поднимает портал ScreenCast + gst→ffmpeg и возвращает
// канал кадров. Это ФОЛБЭК-путь (через CPU-копию/пайп). Нативный путь на GPU
// (libpipewire+libva, cgo) переопределяет waylandVideoFn в cgo-сборке.
// Возвращает nil-канал длительностей: gst-путь пейсит сам (videorate внутри
// пайплайна), потребитель берёт фиксированную 1/fps. Реальные таймстампы — только
// у нативного пути.
func startVideoWaylandGst(ctx context.Context, opts Options) (chan []byte, chan time.Duration, error) {
	gst := gstLaunchPath()
	if gst == "" {
		return nil, nil, fmt.Errorf("gst-launch-1.0 не найден (нужен для Wayland-захвата)")
	}
	// Объединённая портал-сессия (RemoteDesktop+ScreenCast) — синглтон; переживает
	// перезапуски захвата (её же использует ввод). Здесь только берём свежий
	// PipeWire-fd для видео; сессию/conn не закрываем.
	ps, err := ensurePortal()
	if err != nil {
		return nil, nil, err
	}
	node := ps.node
	fd, err := ps.openPipeWire()
	if err != nil {
		return nil, nil, err
	}
	pwFile := os.NewFile(uintptr(fd), "pipewire")

	// 5) gst забирает кадры из PipeWire и отдаёт СЫРОЙ NV12 в stdout; ffmpeg
	// кодирует. Если есть VAAPI (AMD Deck) — ffmpeg делает СКЕЙЛ и КОДИРОВАНИЕ на
	// GPU (h264_vaapi), а gst отдаёт кадр в нативном размере (без софт-скейла).
	// Иначе gst скейлит софтом, ffmpeg кодирует libx264/libvpx.
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	vaapi := opts.Codec == CodecH264 && renderNode() != "" && ffmpegHasVAAPI()

	var gstW, gstH int
	var ffArgs []string
	if vaapi {
		gstW, gstH = captureSize() // нативный размер: скейл сделает GPU в ffmpeg
		tw, th := waylandSize(opts)
		ffArgs = waylandVAAPIArgs(gstW, gstH, tw, th, fps, opts.Bitrate)
	} else {
		gstW, gstH = waylandSize(opts) // софтовый путь: gst скейлит сам
		ffArgs = waylandSoftwareArgs(opts, gstW, gstH, fps)
	}

	gstArgs := gstNV12Pipeline(node, gstW, gstH, fps)
	gstCmd := exec.CommandContext(ctx, gst, gstArgs...)
	gstCmd.ExtraFiles = []*os.File{pwFile} // fd 3 у gst
	log.Printf("capture: gst grab (wayland) node=%d %dx%d vaapi=%v %s", node, gstW, gstH, vaapi, strings.Join(gstArgs, " "))

	ffCmd := exec.CommandContext(ctx, FFmpegPath(), ffArgs...)
	log.Printf("capture: ffmpeg encode (wayland) %s", strings.Join(ffArgs, " "))

	// Связь gst.stdout → ffmpeg.stdin напрямую (kernel pipe, без копирования).
	rp, wp, perr := os.Pipe()
	if perr != nil {
		pwFile.Close()
		return nil, nil, fmt.Errorf("pipe: %w", perr)
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
		return nil, nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}

	if err := gstCmd.Start(); err != nil {
		rp.Close()
		wp.Close()
		pwFile.Close()
		return nil, nil, fmt.Errorf("start gst: %w", err)
	}
	if err := ffCmd.Start(); err != nil {
		rp.Close()
		wp.Close()
		pwFile.Close()
		_ = gstCmd.Process.Kill()
		_ = gstCmd.Wait()
		return nil, nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	// Концы пайпа и fd PipeWire держат уже дочерние процессы — в родителе закрываем.
	rp.Close()
	wp.Close()
	pwFile.Close()

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer waitKill(gstCmd, "video-grab")
		defer waitKill(ffCmd, "video-encode")
		in := bufio.NewReader(ffOut)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()
	return frames, nil, nil
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

// captureSize — нативный размер кадра для gst (разрешение экрана; дефолт 1080p).
func captureSize() (int, int) {
	if sw, sh := ScreenSize(); sw > 0 && sh > 0 {
		return even(sw), even(sh)
	}
	return 1920, 1080
}

// renderNode — DRM render-нода для VAAPI (/dev/dri/renderD128…). Пусто, если нет.
func renderNode() string {
	for i := 128; i < 136; i++ {
		p := fmt.Sprintf("/dev/dri/renderD%d", i)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

var (
	vaapiOnce sync.Once
	vaapiOK   bool
)

// ffmpegHasVAAPI — собран ли ffmpeg с кодером h264_vaapi (кэшируется).
// KATANA_NO_VAAPI=1 принудительно отключает VAAPI (для сравнения с софтом libx264
// — диагностика, кто именно бьёт картинку под движением).
func ffmpegHasVAAPI() bool {
	vaapiOnce.Do(func() {
		if os.Getenv("KATANA_NO_VAAPI") != "" {
			vaapiOK = false
			return
		}
		out, err := exec.Command(FFmpegPath(), "-hide_banner", "-encoders").Output()
		vaapiOK = err == nil && strings.Contains(string(out), "h264_vaapi")
	})
	return vaapiOK
}

// gstNV12Pipeline — gst забирает PipeWire, приводит к NV12 (фикс. WxH) и пишет
// сырьё в stdout. Никакого кодирования в gst (в этой сборке нет VA-энкодеров).
func gstNV12Pipeline(node uint32, w, h, fps int) []string {
	desc := fmt.Sprintf(
		"pipewiresrc fd=3 path=%d do-timestamp=true keepalive-time=1000 ! "+
			// БЕЗ leaky-queue: кадры идут ПОДРЯД. Дроп промежуточных кадров под
			// движением давал энкодеру скачок между кадрами → окно рассыпалось.
			"videorate ! videoconvert n-threads=0 ! videoscale n-threads=0 ! "+
			"video/x-raw,format=NV12,width=%d,height=%d,framerate=%d/1 ! "+
			"fdsink fd=1 sync=false",
		node, w, h, fps)
	// Токенами (не единой строкой) — иначе gst-launch: syntax error. -q гасит
	// прогресс в stdout (туда fdsink пишет сырьё).
	return append([]string{"-q"}, strings.Fields(desc)...)
}

// waylandVAAPIArgs — ffmpeg читает сырой NV12 (cw×ch) из stdin, грузит в GPU,
// скейлит и кодирует H264 аппаратно (h264_vaapi). Почти без нагрузки на CPU.
func waylandVAAPIArgs(cw, ch, tw, th, fps int, bitrate string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-init_hw_device", "vaapi=va:" + renderNode(), "-filter_hw_device", "va",
		"-f", "rawvideo", "-pixel_format", "nv12",
		"-video_size", fmt.Sprintf("%dx%d", cw, ch),
		"-framerate", fmt.Sprintf("%d", fps),
		"-i", "-",
		"-r", fmt.Sprintf("%d", fps), "-fps_mode", "cfr",
		"-vf", fmt.Sprintf("hwupload,scale_vaapi=w=%d:h=%d", tw, th),
		// VBR с большим потолком (до 3× таргета): под резким движением кадру нужно
		// куда больше бит — даём разгуляться, чтобы качество не рушилось в блоки.
		// bf 0 — без B-кадров (низкая задержка).
		"-c:v", "h264_vaapi", "-rc_mode", "VBR",
		"-b:v", bitrate,
		"-maxrate", fmt.Sprintf("%dk", bitrateKbps(bitrate)*3),
		"-bufsize", fmt.Sprintf("%dk", bitrateKbps(bitrate)*3),
		"-g", fmt.Sprintf("%d", fps), "-bf", "0",
		"-f", "h264", "pipe:1",
	}
}

// waylandSoftwareArgs — фолбэк без VAAPI: ffmpeg читает сырой NV12 (w×h) и
// кодирует софтом (libx264 / libvpx).
func waylandSoftwareArgs(opts Options, w, h, fps int) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "rawvideo", "-pixel_format", "nv12",
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
