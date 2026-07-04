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
	"time"

	"bufio"
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

// startAudioProc запускает аудио-ffmpeg (PulseAudio → Opus) и возвращает канал
// Opus-пакетов. @DEFAULT_MONITOR@ = монитор дефолтного sink (СИСТЕМНЫЙ вывод), а
// не микрофон.
func startAudioProc(ctx context.Context) (chan []byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostats",
		// Малый буфер захвата PulseAudio (низкая латентность, без «наплыва» пакетов).
		"-fragment_size", "1920", // 48000*2ch*2B*20мс = 3840 байт кадра; фрагмент ~20мс
		"-f", "pulse", "-i", "@DEFAULT_MONITOR@",
		// Ровно 48к/стерео — совпадает с RTP-часами Opus у зрителя. application=audio
		// (не lowdelay: тот CELT-only, звучит «булькающе/водянисто» на музыке/системном
		// звуке). frame_duration=20 → один Opus-кадр = один RTP-пакет = 20 мс.
		"-ac", "2", "-ar", "48000",
		"-c:a", "libopus", "-b:a", "128k", "-vbr", "on",
		"-application", "audio", "-frame_duration", "20",
		"-page_duration", "20000", "-flush_packets", "1",
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

// readOggOpus читает ogg-поток и шлёт в канал ОТДЕЛЬНЫЕ Opus-пакеты (по 20 мс).
// КРИТИЧНО: страница ogg может нести НЕСКОЛЬКО пакетов (ffmpeg иногда пакует 2).
// Если слать страницу целиком как один RTP-сэмпл — склейка невалидна для декодера
// («бульк») и RTP-метка отстаёт от реального звука (20 мс меток на 40 мс аудио) →
// накопительный рассинхрон, браузер то ускоряет, то тормозит. Поэтому парсим
// lacing-таблицу страницы и режем на пакеты — как на маке (libopus отдаёт по
// одному пакету). Заголовки OpusHead/OpusTags пропускаем.
func readOggOpus(ctx context.Context, r io.Reader, out chan []byte) {
	defer close(out)
	br := bufio.NewReader(r)
	var cont []byte // пакет, продолжающийся с прошлой страницы (lacing 255 на конце)
	// Диагностика прихода: пакеты и разрывы >40 мс (норма — ровно 20 мс между
	// пакетами), сводка раз в ~10 с; ~500 пакетов/10с = захват здоров.
	var n, gaps int
	var maxGap time.Duration
	last := time.Now()
	statT := last
	for {
		pkts, err := parseOggPage(br)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("audio: ogg read: %v", err)
			}
			return
		}
		for i, p := range pkts.packets {
			if len(cont) > 0 {
				p = append(cont, p...)
				cont = nil
			}
			if i == len(pkts.packets)-1 && pkts.lastPartial {
				cont = append([]byte(nil), p...) // не завершён — доклеим со след. страницы
				break
			}
			if len(p) >= 8 && (string(p[:8]) == "OpusHead" || string(p[:8]) == "OpusTags") {
				continue
			}
			now := time.Now()
			if d := now.Sub(last); n > 0 {
				if d > 40*time.Millisecond {
					gaps++
				}
				if d > maxGap {
					maxGap = d
				}
			}
			last = now
			n++
			if now.Sub(statT) >= 10*time.Second {
				log.Printf("audio: arrival n=%d gaps>40ms=%d max=%.0fms (10s)", n, gaps, maxGap.Seconds()*1000)
				n, gaps, maxGap = 0, 0, 0
				statT = now
			}
			select {
			case out <- p:
			case <-ctx.Done():
				return
			}
		}
	}
}

// oggPage — пакеты одной ogg-страницы; lastPartial=true, если последний пакет
// не завершён (lacing 255 в конце) и продолжается на следующей странице.
type oggPage struct {
	packets     [][]byte
	lastPartial bool
}

// parseOggPage читает одну страницу ogg (RFC 3533): заголовок 27 байт "OggS...",
// таблица lacing-значений (255 = пакет продолжается), payload. Режет payload на
// пакеты по lacing.
func parseOggPage(br *bufio.Reader) (oggPage, error) {
	var pg oggPage
	var hdr [27]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return pg, err
	}
	if string(hdr[:4]) != "OggS" {
		return pg, fmt.Errorf("bad ogg capture pattern")
	}
	nseg := int(hdr[26])
	lacing := make([]byte, nseg)
	if _, err := io.ReadFull(br, lacing); err != nil {
		return pg, err
	}
	total := 0
	for _, l := range lacing {
		total += int(l)
	}
	payload := make([]byte, total)
	if _, err := io.ReadFull(br, payload); err != nil {
		return pg, err
	}
	off, start := 0, 0
	for _, l := range lacing {
		off += int(l)
		if l < 255 { // пакет завершён
			pg.packets = append(pg.packets, payload[start:off])
			start = off
		}
	}
	if start < total { // хвост без терминатора — продолжится на след. странице
		pg.packets = append(pg.packets, payload[start:])
		pg.lastPartial = true
	}
	return pg, nil
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
