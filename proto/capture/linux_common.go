//go:build linux

package capture

// Захват на Linux, общая часть: детект окружения, выбор видео-бэкенда по сессии
// (X11 → x11grab в backend_x11grab_linux.go; Wayland → портал+PipeWire→VAAPI в
// backend_portal_vaapi_linux.* или fallback gst в backend_wayland_gst_linux.go) и
// вспомогалки (разрешение экрана, источники). Звук — в audio_pulse_linux.go.
// Видео и звук — независимые процессы: сбой одного не роняет другой.
//
// Целевой стек и матрица поддержки (KDE/AMD/Wayland) — в doc.go. Здесь же —
// runtime-детект (describeEnv) и честный лог выбранного бэкенда.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// isWayland — текущая графическая сессия Wayland (тогда x11grab не годится).
func isWayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

// IsWaylandSession — Wayland-сессия (ввод идёт через портал, а не uinput).
func IsWaylandSession() bool { return isWayland() }

// videoBackend — какой бэкенд видео использовать:
//
//	"wayland" — Wayland-сессия: портал ScreenCast + PipeWire (нужен gst-launch);
//	"x11"     — X11-десктоп ($DISPLAY): x11grab (нужен ffmpeg);
//	""        — видео недоступно (headless / нет нужных бинарей).
func videoBackend() string {
	if isWayland() {
		if gstLaunchPath() != "" && FFmpegPath() != "" {
			return "wayland" // gst забирает PipeWire, ffmpeg кодирует
		}
		return "" // без gst или ffmpeg видео не снять
	}
	if os.Getenv("DISPLAY") != "" && FFmpegPath() != "" {
		return "x11"
	}
	return ""
}

// VideoAvailable — есть ли рабочий видео-бэкенд.
func VideoAvailable() bool { return videoBackend() != "" }

// desktopEnv — окружение рабочего стола из XDG_CURRENT_DESKTOP ("KDE"/"GNOME"/…),
// в нижнем регистре; "" если не задано. Портал RemoteDesktop (ввод) есть только
// у KDE/GNOME — см. матрицу в doc.go.
func desktopEnv() string {
	de := os.Getenv("XDG_CURRENT_DESKTOP")
	if de == "" {
		de = os.Getenv("XDG_SESSION_DESKTOP")
	}
	// XDG_CURRENT_DESKTOP может быть списком "KDE:plasma" — берём первый.
	if i := strings.IndexAny(de, ":;"); i > 0 {
		de = de[:i]
	}
	return strings.ToLower(strings.TrimSpace(de))
}

// gpuVendor — вендор первой видеокарты из /sys/class/drm по PCI vendor id.
// DMABUF→VAAPI zero-copy tuned под AMD; см. doc.go. "" если не определить.
func gpuVendor() string {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "card") || strings.Contains(e.Name(), "-") {
			continue
		}
		v, err := os.ReadFile(filepath.Join("/sys/class/drm", e.Name(), "device", "vendor"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(v)) {
		case "0x1002":
			return "AMD"
		case "0x8086":
			return "Intel"
		case "0x10de":
			return "NVIDIA"
		default:
			return strings.TrimSpace(string(v))
		}
	}
	return ""
}

// describeEnv — строка окружения для лога («session=… desktop=… gpu=… backend=…»)
// плюс предупреждение, если стек вне целевого профиля (не KDE/GNOME или NVIDIA).
// Делает зависимость от KDE/AMD видимой в рантайме, а не только в исходниках.
func describeEnv(backend string) string {
	session := "x11"
	if isWayland() {
		session = "wayland"
	}
	de, gpu := desktopEnv(), gpuVendor()
	msg := fmt.Sprintf("session=%s desktop=%s gpu=%s backend=%s", session, orNA(de), orNA(gpu), orNA(backend))
	var warn []string
	if backend == "wayland" && de != "kde" && de != "gnome" && de != "" {
		warn = append(warn, "портал RemoteDesktop (ввод мыши) есть только у KDE/GNOME — на "+de+" ввод может не работать")
	}
	if gpu == "NVIDIA" {
		warn = append(warn, "NVIDIA: VAAPI-энкод не поддержан, ожидается фолбэк/сбой видео")
	}
	if len(warn) > 0 {
		msg += " | WARN: " + strings.Join(warn, "; ")
	}
	return msg
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
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

// waylandVideoFn — функция захвата видео на Wayland. По умолчанию — gst→ffmpeg
// (через CPU/пайп). В cgo-сборке init() в video_wayland_native_linux.go
// переопределяет её на нативный путь (libpipewire+libva, кадр на GPU).
var waylandVideoFn = startVideoWaylandGst

// Хуки нативного энкодера для WebRTC-контура: форс IDR (ответ на PLI зрителя) и
// смена битрейта на лету (адаптация к сети). Задаёт нативный путь; gst-путь
// сбрасывает в nil (не поддерживает). Без них любая потеря пакета сыпет картинку
// до планового IDR, а битрейт не подстраивается под канал.
var (
	waylandForceKey   func()
	waylandSetBitrate func(kbps int)
)

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
	log.Printf("capture: %s audio=%v", describeEnv(backend), audio)
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
		v, err := waylandVideoFn(ctx, opts)
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
	st := &Stream{Video: video, Audio: audioCh}
	if backend == "wayland" {
		// Нативный путь умеет PLI→IDR и смену битрейта; gst-путь оставляет nil.
		st.ForceKeyframe = waylandForceKey
		st.SetBitrate = waylandSetBitrate
	}
	return st, nil
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

// waitKill гарантирует остановку subprocess и логирует завершение.
func waitKill(cmd *exec.Cmd, what string) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	log.Printf("capture stopped (%s)", what)
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
