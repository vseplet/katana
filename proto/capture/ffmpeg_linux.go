//go:build linux

package capture

// Захват экрана и системного звука на Linux через ffmpeg: видео — x11grab,
// звук — PulseAudio (-f pulse). Всё capability-driven: без графики ($DISPLAY)
// хост остаётся headless (только терминал), без PulseAudio — без звука. Формат
// выхода тот же, что и на macOS (IVF/VP8 или Annex-B/H264 + Opus в ogg), поэтому
// чтение (readIVF/readH264/oggreader) и WebRTC-путь общие.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// VideoAvailable — можно ли захватывать экран: нужен X11-дисплей ($DISPLAY) и
// найденный ffmpeg. На headless-сервере (нет графики) — false, и хост поднимется
// только с терминалом. Реального кадра это не гарантирует (иксы могут не пустить)
// — тогда ffmpeg отвалится и видео-канал закроется, терминал продолжит работать.
func VideoAvailable() bool {
	return os.Getenv("DISPLAY") != "" && FFmpegPath() != ""
}

// AudioAvailable — доступен ли системный звук: PulseAudio (или PipeWire-pulse) и
// ffmpeg. Проверяем переменную PULSE_SERVER и стандартный per-user сокет. Без
// звукового сервера (headless) — false.
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

// x11Display — адрес входа x11grab. Берём $DISPLAY (напр. ":0"); x11grab сам
// определяет геометрию корневого окна, если -video_size не задан.
func x11Display() string {
	d := os.Getenv("DISPLAY")
	if d == "" {
		d = ":0"
	}
	return d
}

// NewEncoder на Linux: ffmpeg (x11grab + pulse), если есть графика; иначе —
// headless-заглушка без видео (терминал продолжает работать).
func NewEncoder() CaptureEncoder {
	if !VideoAvailable() {
		return noVideoEncoder{}
	}
	return &FFmpegLinux{}
}

// FFmpegLinux реализует CaptureEncoder через ffmpeg на Linux.
type FFmpegLinux struct{}

// buildLinuxArgs собирает аргументы ffmpeg: x11grab (или тест-источник) для видео
// и, если audio, второй вход -f pulse. Видео уходит в stdout (pipe:1), звук —
// в pipe:3 (ExtraFiles). Возвращает аргументы и признак, что звук включён.
func buildLinuxArgs(opts Options, audio bool) ([]string, bool) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	if opts.TestSource {
		// Синтетический движущийся источник — отладка без графики.
		w := opts.Width
		if w <= 0 {
			w = 1280
		}
		args = append(args,
			"-re",
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%dx720:rate=%d", w, opts.FPS),
		)
	} else {
		draw := "0"
		if opts.Cursor {
			draw = "1"
		}
		args = append(args,
			"-f", "x11grab",
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-draw_mouse", draw,
			"-i", x11Display(),
		)
	}
	if audio {
		args = append(args, "-f", "pulse", "-i", "default")
		args = append(args, "-map", "0:v")
	}
	args = append(args, linuxVideoOut(opts)...)
	if audio {
		// Opus, страница ≈ 20 мс (совпадает с Duration в WriteSample).
		args = append(args,
			"-map", "1:a",
			"-c:a", "libopus", "-b:a", "128k", "-application", "lowdelay",
			"-page_duration", "20000",
			"-f", "ogg", "pipe:3",
		)
	}
	return args, audio
}

// linuxVideoOut — общая часть видеовыхода: CFR, опц. даунскейл, энкодер. H264 —
// софтовый libx264 (VideoToolbox только на Apple); VP8 — libvpx, как на macOS.
func linuxVideoOut(opts Options) []string {
	a := []string{"-r", fmt.Sprintf("%d", opts.FPS), "-fps_mode", "cfr"}
	if opts.Width > 0 {
		a = append(a, "-vf", fmt.Sprintf("scale=%d:-2", opts.Width))
	}
	a = append(a, "-pix_fmt", "yuv420p")

	if opts.Codec == CodecH264 {
		a = append(a,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "high",
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			// SPS/PPS перед каждым кейфреймом — чтобы новый зритель декодировал.
			"-bsf:v", "dump_extra=freq=keyframe",
			"-f", "h264", "pipe:1",
		)
	} else {
		a = append(a,
			"-c:v", "libvpx",
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-lag-in-frames", "0",
			"-threads", fmt.Sprintf("%d", opts.Threads),
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-keyint_min", fmt.Sprintf("%d", opts.FPS),
			"-f", "ivf", "pipe:1",
		)
	}
	return a
}

// Start запускает ffmpeg-захват и возвращает каналы кадров (видео + опц. звук).
func (f *FFmpegLinux) Start(ctx context.Context, opts Options) (*Stream, error) {
	ff := FFmpegPath()
	if ff == "" {
		return nil, errNoFFmpeg
	}
	// Звук передаём, только если его просят И PulseAudio реально доступен.
	audio := opts.Audio && AudioAvailable()
	args, wantAudio := buildLinuxArgs(opts, audio)

	cmd := exec.CommandContext(ctx, ff, args...)
	log.Printf("capture: ffmpeg %s", strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)

	// Звук ffmpeg пишет в fd 3 (pipe:3) — прокидываем через ExtraFiles.
	var audioR *os.File
	var audioW *os.File
	var audioCh chan []byte
	if wantAudio {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return nil, fmt.Errorf("audio pipe: %w", perr)
		}
		cmd.ExtraFiles = []*os.File{pw} // становится fd 3 у ffmpeg
		audioR, audioW = pr, pw
		audioCh = make(chan []byte, 16)
	}

	if err := cmd.Start(); err != nil {
		if audioR != nil {
			_ = audioR.Close()
			_ = audioW.Close()
		}
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	if audioW != nil {
		_ = audioW.Close() // копию держит ffmpeg; родитель в fd 3 не пишет
	}

	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			if audioR != nil {
				_ = audioR.Close()
			}
			log.Printf("capture stopped")
		}()
		in := bufio.NewReader(stdout)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()

	if wantAudio {
		go readOggOpus(ctx, audioR, audioCh)
	}

	return &Stream{Video: frames, Audio: audioCh}, nil
}

// readOggOpus читает Opus-страницы из ogg-потока ffmpeg (fd 3) и шлёт пакеты в
// канал. Каждая страница ≈ 20 мс (см. -page_duration).
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

// Источники/ввод на Linux пока не реализованы (нет перечисления окон и инъекции
// скролла) — заглушки, симметрично stub_other.go для прочих платформ.
func ListSources() (Sources, error)            { return Sources{}, nil }
func ActivateApp(_ int) error                  { return nil }
func InjectScroll(_, _ int)                    {}
func SourceRect(_ string, _ int) (Rect, error) { return Rect{}, nil }
