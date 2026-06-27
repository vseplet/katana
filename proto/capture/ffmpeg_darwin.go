package capture

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

// FFmpegDarwin реализует CaptureEncoder через ffmpeg + avfoundation на macOS.
//
// ffmpeg захватывает экран целиком, кодирует в VP8 (libvpx, realtime)
// и выдаёт IVF-поток в stdout. Go читает поток через ivfreader и отдаёт
// VP8-кадры в канал.
type FFmpegDarwin struct{}

// NewFFmpegDarwin создаёт ffmpeg-бэкенд захвата для macOS.
func NewFFmpegDarwin() *FFmpegDarwin {
	return &FFmpegDarwin{}
}

// buildArgs собирает аргументы ffmpeg из опций. Вынесено отдельно для читаемости.
func buildArgs(opts Options) []string {
	var args []string
	if opts.TestSource {
		// Синтетический движущийся источник — отладка без TCC.
		w := opts.Width
		if w <= 0 {
			w = 1280
		}
		args = []string{
			"-re", // отдавать в реальном темпе, иначе lavfi сыплет кадры быстрее времени
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%dx720:rate=%d", w, opts.FPS),
		}
	} else {
		args = []string{
			"-f", "avfoundation",
			"-capture_cursor", "1",
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-i", fmt.Sprintf("%d:", opts.ScreenIndex), // "<index>:" = экран, без аудио
		}
		// Даунскейл с Retina — иначе software-VP8 жжёт CPU.
		if opts.Width > 0 {
			args = append(args, "-vf", fmt.Sprintf("scale=%d:-2", opts.Width))
		}
	}

	args = append(args,
		// Форсируем постоянную частоту кадров (CFR) на выходе. avfoundation
		// отдаёт кадры с переменной частотой (VFR) и реальными таймстемпами;
		// без CFR реальная каденция расходится с фиксированной Duration в
		// WriteSample → RTP-часы уплывают → браузер замирает после 1-го кадра.
		"-r", fmt.Sprintf("%d", opts.FPS),
		"-fps_mode", "cfr",
		"-pix_fmt", "yuv420p", // libvpx требует yuv420p (вход uyvy422)
		"-c:v", "libvpx",
		"-deadline", "realtime",
		"-cpu-used", "8",
		"-lag-in-frames", "0", // без lookahead — убирает скрытую задержку энкодера
		"-b:v", opts.Bitrate,
		// кейфрейм раз в секунду: новый зритель быстро получает картинку
		// (компенсация отсутствия PLI-on-demand в прототипе).
		"-g", fmt.Sprintf("%d", opts.FPS),
		"-keyint_min", fmt.Sprintf("%d", opts.FPS),
		"-f", "ivf",
		"-", // вывод в stdout
	)
	return args
}

// Start запускает ffmpeg и возвращает канал VP8-кадров.
func (f *FFmpegDarwin) Start(ctx context.Context, opts Options) (<-chan []byte, error) {
	args := buildArgs(opts)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	log.Printf("capture: ffmpeg %s", strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	// stderr ffmpeg уводим в логи построчно — полезно для диагностики
	// (TCC, индекс экрана, чёрный кадр и т.п.).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	go logStderr(stderr)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	log.Printf("capture started: screen=%d width=%d fps=%d bitrate=%s",
		opts.ScreenIndex, opts.Width, opts.FPS, opts.Bitrate)

	frames := make(chan []byte, 32)
	go func() {
		defer close(frames)
		defer func() {
			// Гарантируем, что subprocess не осиротеет.
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			log.Printf("capture stopped")
		}()

		// ivfreader парсит IVF-заголовок и отдаёт кадры по одному.
		// Делаем это в горутине: чтение заголовка ждёт инициализации
		// ffmpeg и не должно задерживать сигналинг.
		reader, _, err := ivfreader.NewWith(bufio.NewReader(stdout))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: ivf header: %v", err)
			}
			return
		}

		for {
			frame, _, err := reader.ParseNextFrame()
			if err != nil {
				// ctx отменён или ffmpeg завершился — нормальный конец потока.
				if ctx.Err() == nil {
					log.Printf("capture: read frame: %v", err)
				}
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	return frames, nil
}

// logStderr построчно льёт stderr ffmpeg в стандартный лог.
func logStderr(r interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Printf("ffmpeg: %s", sc.Text())
	}
}
