//go:build linux

package capture

// Захват экрана и системного звука на Linux через ffmpeg. Видео-бэкенд выбирается
// по сессии: X11 → x11grab; Wayland/gamescope → kmsgrab (снимаем финальный
// композит прямо с DRM-сканаута — видит и Wayland-окна, и игры). Звук — PulseAudio
// (монитор дефолтного sink). Видео и звук — ДВА независимых ffmpeg-процесса, чтобы
// сбой одного (напр. нет прав на DRM для kmsgrab) не ронял другой. Формат выхода
// тот же, что на macOS (Annex-B/H264 или IVF/VP8 + Opus в ogg) → чтение
// (readH264/readIVF/oggreader) и WebRTC-путь общие.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// videoBackend — какой ffmpeg-вход использовать для видео:
//
//	"x11grab" — X11-десктоп ($DISPLAY, не Wayland);
//	"kmsgrab" — Wayland/gamescope: композит с DRM-сканаута. Нужен доступ к /dev/dri
//	            и права на DRM (CAP_SYS_ADMIN у ffmpeg) — иначе процесс упадёт;
//	""        — видео недоступно (нет графики/ffmpeg) → headless.
func videoBackend() string {
	if FFmpegPath() == "" {
		return ""
	}
	wayland := os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
	if !wayland && os.Getenv("DISPLAY") != "" {
		return "x11grab" // чистый X11 — снимаем root
	}
	if drmCard() != "" {
		return "kmsgrab" // Wayland/gamescope или без X, но есть DRM
	}
	if os.Getenv("DISPLAY") != "" {
		return "x11grab" // последний фолбэк (Xwayland без DRM-доступа)
	}
	return ""
}

// VideoAvailable — есть ли рабочий видео-бэкенд (x11grab или kmsgrab) + ffmpeg.
func VideoAvailable() bool { return videoBackend() != "" }

// drmCard — первая DRM-карта (/dev/dri/card0…7). Пусто, если DRM нет (headless).
func drmCard() string {
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("/dev/dri/card%d", i)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// AudioAvailable — доступен ли системный звук: PulseAudio (или PipeWire-pulse) и
// ffmpeg. Проверяем PULSE_SERVER и стандартный per-user сокет.
func AudioAvailable() bool {
	if FFmpegPath() == "" {
		return false
	}
	if os.Getenv("PULSE_SERVER") != "" {
		return true
	}
	sock := filepath.Join(fmt.Sprintf("/run/user/%d", os.Getuid()), "pulse", "native")
	_, err := os.Stat(sock)
	return err == nil
}

// x11Display — адрес входа x11grab ($DISPLAY, напр. ":0"); x11grab сам определяет
// геометрию корневого окна.
func x11Display() string {
	d := os.Getenv("DISPLAY")
	if d == "" {
		d = ":0"
	}
	return d
}

// NewEncoder на Linux: ffmpeg-энкодер, если доступно видео ИЛИ звук; иначе
// headless-заглушка (только терминал).
func NewEncoder() CaptureEncoder {
	if !VideoAvailable() && !AudioAvailable() {
		return noVideoEncoder{}
	}
	return &FFmpegLinux{}
}

// FFmpegLinux реализует CaptureEncoder через ffmpeg на Linux.
type FFmpegLinux struct{}

// Start поднимает видео- и/или аудио-процессы (независимо) и возвращает каналы.
func (f *FFmpegLinux) Start(ctx context.Context, opts Options) (*Stream, error) {
	if FFmpegPath() == "" {
		return nil, errNoFFmpeg
	}
	backend := videoBackend()
	audio := opts.Audio && AudioAvailable()
	if backend == "" && !audio {
		return noVideoEncoder{}.Start(ctx, opts) // ни видео, ни звука → headless
	}

	var video chan []byte
	if backend != "" {
		v, err := startVideoProc(ctx, opts, backend)
		if err != nil {
			log.Printf("capture: video (%s): %v (continuing without video)", backend, err)
			video = closedChan()
		} else {
			video = v
		}
	} else {
		video = closedChan()
	}

	var audioCh chan []byte
	if audio {
		a, err := startAudioProc(ctx)
		if err != nil {
			log.Printf("capture: audio: %v (continuing without audio)", err)
		} else {
			audioCh = a
		}
	}

	return &Stream{Video: video, Audio: audioCh}, nil
}

func closedChan() chan []byte {
	ch := make(chan []byte)
	close(ch)
	return ch
}

// buildVideoArgs — аргументы ffmpeg для видео (по бэкенду) с выходом H264/VP8 в
// stdout. kmsgrab отдаёт DRM_PRIME-кадры → hwdownload в CPU-формат перед энкодом.
func buildVideoArgs(opts Options, backend string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	kms := backend == "kmsgrab"

	switch {
	case opts.TestSource:
		w := opts.Width
		if w <= 0 {
			w = 1280
		}
		args = append(args, "-re", "-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%dx720:rate=%d", w, opts.FPS))
	case kms:
		args = append(args,
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-device", drmCard(),
			"-f", "kmsgrab", "-i", "-")
	default: // x11grab
		draw := "0"
		if opts.Cursor {
			draw = "1"
		}
		args = append(args,
			"-f", "x11grab",
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-draw_mouse", draw,
			"-i", x11Display())
	}

	args = append(args, "-r", fmt.Sprintf("%d", opts.FPS), "-fps_mode", "cfr")

	// Видео-фильтр: для kmsgrab сначала скачиваем кадр из DRM в CPU (bgr0),
	// затем опциональный даунскейл.
	var vf []string
	if kms {
		vf = append(vf, "hwdownload", "format=bgr0")
	}
	if opts.Width > 0 {
		vf = append(vf, fmt.Sprintf("scale=%d:-2", opts.Width))
	}
	if len(vf) > 0 {
		args = append(args, "-vf", strings.Join(vf, ","))
	}
	args = append(args, "-pix_fmt", "yuv420p")

	if opts.Codec == CodecH264 {
		args = append(args,
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "high", "-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-bsf:v", "dump_extra=freq=keyframe",
			"-f", "h264", "pipe:1")
	} else {
		args = append(args,
			"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8",
			"-lag-in-frames", "0", "-threads", fmt.Sprintf("%d", opts.Threads),
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-keyint_min", fmt.Sprintf("%d", opts.FPS),
			"-f", "ivf", "pipe:1")
	}
	return args
}

// startVideoProc запускает видео-ffmpeg и возвращает канал кадров.
func startVideoProc(ctx context.Context, opts Options, backend string) (chan []byte, error) {
	args := buildVideoArgs(opts, backend)
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	log.Printf("capture: ffmpeg video (%s) %s", backend, strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			log.Printf("capture stopped (video)")
		}()
		in := bufio.NewReader(stdout)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()
	return frames, nil
}

// startAudioProc запускает аудио-ffmpeg (PulseAudio → Opus) и возвращает канал
// Opus-пакетов. @DEFAULT_MONITOR@ = монитор дефолтного sink (СИСТЕМНЫЙ вывод), а
// не микрофон (-i default брал бы источник по умолчанию = вход/шум).
func startAudioProc(ctx context.Context) (chan []byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		"-f", "pulse", "-i", "@DEFAULT_MONITOR@",
		"-c:a", "libopus", "-b:a", "128k", "-application", "lowdelay",
		"-page_duration", "20000",
		"-f", "ogg", "pipe:1",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath(), args...)
	log.Printf("capture: ffmpeg audio %s", strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	out := make(chan []byte, 16)
	go func() {
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			log.Printf("capture stopped (audio)")
		}()
		readOggOpus(ctx, stdout, out)
	}()
	return out, nil
}

// readOggOpus читает Opus-страницы из ogg-потока ffmpeg и шлёт пакеты в канал.
// Каждая страница ≈ 20 мс (см. -page_duration). Закрывает канал по завершении.
func readOggOpus(ctx context.Context, r io.Reader, out chan []byte) {
	defer close(out)
	reader, _, err := oggreader.NewWith(bufio.NewReader(r))
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("capture: ogg header: %v", err)
		}
		return
	}
	for {
		page, _, err := reader.ParseNextPage()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("audio: ogg read: %v", err)
			}
			return
		}
		select {
		case out <- page:
		case <-ctx.Done():
			return
		}
	}
}

// Перечисление окон/приложений на Linux не реализовано (захват — весь экран).
func ListSources() (Sources, error) { return Sources{}, nil }
func ActivateApp(_ int) error       { return nil }
func InjectScroll(_, _ int)         {} // скролл на Linux идёт через uinput (input_linux.go)

// SourceRect на Linux — весь экран. Нужен для маппинга нормализованных координат
// мыши зрителя в пиксели (см. handleMouse) и как ABS-диапазон uinput.
func SourceRect(_ string, _ int) (Rect, error) {
	w, h := ScreenSize()
	if w <= 0 || h <= 0 {
		return Rect{}, nil
	}
	return Rect{X: 0, Y: 0, W: float64(w), H: float64(h)}, nil
}

// ScreenSize определяет разрешение подключённого дисплея из DRM sysfs
// (/sys/class/drm/<connector>/{status,modes}) — работает и в X11, и в Wayland.
// 0,0 если не удалось.
func ScreenSize() (int, int) {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.Contains(name, "-") { // коннекторы: card0-HDMI-A-1 и т.п.
			continue
		}
		st, err := os.ReadFile(filepath.Join("/sys/class/drm", name, "status"))
		if err != nil || strings.TrimSpace(string(st)) != "connected" {
			continue
		}
		modes, err := os.ReadFile(filepath.Join("/sys/class/drm", name, "modes"))
		if err != nil {
			continue
		}
		first := strings.SplitN(strings.TrimSpace(string(modes)), "\n", 2)[0]
		if w, h, ok := parseMode(first); ok {
			return w, h
		}
	}
	return 0, 0
}

// parseMode разбирает строку режима вида "3840x2160" (возможен суффикс, напр. "i").
func parseMode(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, 'x')
	if i <= 0 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(s[:i])
	rest := s[i+1:]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	h, err2 := strconv.Atoi(rest[:j])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
