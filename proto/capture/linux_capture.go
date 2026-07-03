//go:build linux

package capture

// Захват на Linux, общая часть: выбор видео-бэкенда по сессии (X11 → x11grab в
// video_x11_linux.go; Wayland → xdg-desktop-portal ScreenCast + PipeWire в
// video_wayland_linux.go), захват звука (PulseAudio → Opus) и вспомогалки
// (разрешение экрана, источники). Видео и звук — независимые процессы: сбой
// одного не роняет другой.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"bufio"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// isWayland — текущая графическая сессия Wayland (тогда x11grab не годится).
func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

// videoBackend — какой бэкенд видео использовать:
//
//	"wayland" — Wayland-сессия: портал ScreenCast + PipeWire (нужен gst-launch);
//	"x11"     — X11-десктоп ($DISPLAY): x11grab (нужен ffmpeg);
//	""        — видео недоступно (headless / нет нужных бинарей).
func videoBackend() string {
	if isWayland() {
		if gstLaunchPath() != "" {
			return "wayland"
		}
		return "" // Wayland без GStreamer — видео не снять
	}
	if os.Getenv("DISPLAY") != "" && FFmpegPath() != "" {
		return "x11"
	}
	return ""
}

// VideoAvailable — есть ли рабочий видео-бэкенд.
func VideoAvailable() bool { return videoBackend() != "" }

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

// gstLaunchPath — путь к gst-launch-1.0 (для Wayland-видео из PipeWire). Сначала
// рядом (~/.katana/bin), затем в PATH. Пусто, если не найден.
func gstLaunchPath() string {
	name := "gst-launch-1.0"
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".katana", "bin", name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// NewEncoder на Linux: ffmpeg/gst-энкодер, если доступно видео ИЛИ звук; иначе
// headless-заглушка (только терминал).
func NewEncoder() CaptureEncoder {
	if !VideoAvailable() && !AudioAvailable() {
		return noVideoEncoder{}
	}
	return &FFmpegLinux{}
}

// FFmpegLinux — Linux-энкодер (имя историческое; на Wayland видео идёт через gst).
type FFmpegLinux struct{}

// Start поднимает видео- и/или аудио-процессы (независимо) и возвращает каналы.
func (f *FFmpegLinux) Start(ctx context.Context, opts Options) (*Stream, error) {
	backend := videoBackend()
	audio := opts.Audio && AudioAvailable()
	if backend == "" && !audio {
		return noVideoEncoder{}.Start(ctx, opts)
	}

	var video chan []byte
	switch backend {
	case "x11":
		v, err := startVideoX11(ctx, opts)
		if err != nil {
			log.Printf("capture: video (x11): %v (continuing without video)", err)
			video = closedChan()
		} else {
			video = v
		}
	case "wayland":
		v, err := startVideoWayland(ctx, opts)
		if err != nil {
			log.Printf("capture: video (wayland/portal): %v (continuing without video)", err)
			video = closedChan()
		} else {
			video = v
		}
	default:
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

// bitrateKbps парсит opts.Bitrate ("3M"/"3000k"/"3000") в kbps (дефолт 3000).
func bitrateKbps(b string) int {
	b = strings.TrimSpace(strings.ToLower(b))
	mult := 1
	switch {
	case strings.HasSuffix(b, "m"):
		mult, b = 1000, strings.TrimSuffix(b, "m")
	case strings.HasSuffix(b, "k"):
		mult, b = 1, strings.TrimSuffix(b, "k")
	}
	n, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil || n <= 0 {
		return 3000
	}
	return n * mult
}

// startAudioProc запускает аудио-ffmpeg (PulseAudio → Opus) и возвращает канал
// Opus-пакетов. @DEFAULT_MONITOR@ = монитор дефолтного sink (СИСТЕМНЫЙ вывод), а
// не микрофон.
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
		defer waitKill(cmd, "audio")
		readOggOpus(ctx, stdout, out)
	}()
	return out, nil
}

// waitKill гарантирует остановку subprocess и логирует завершение.
func waitKill(cmd *exec.Cmd, what string) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	log.Printf("capture stopped (%s)", what)
}

// readOggOpus читает Opus-страницы из ogg-потока и шлёт пакеты в канал (≈20 мс).
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
func InjectScroll(_, _ int)         {} // скролл на Linux — через ввод (uinput/портал)

// SourceRect на Linux — весь экран. Нужен для маппинга нормализованных координат
// мыши зрителя в пиксели.
func SourceRect(_ string, _ int) (Rect, error) {
	w, h := ScreenSize()
	if w <= 0 || h <= 0 {
		return Rect{}, nil
	}
	return Rect{X: 0, Y: 0, W: float64(w), H: float64(h)}, nil
}

// ScreenSize определяет разрешение подключённого дисплея из DRM sysfs. 0,0 если
// не удалось.
func ScreenSize() (int, int) {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.Contains(name, "-") {
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
