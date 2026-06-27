package capture

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
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
	// Глушим вывод ffmpeg: без баннера, только ошибки, без строки прогресса —
	// иначе stdout-лог сервера засоряется на каждый (пере)запуск.
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	if opts.TestSource {
		// Синтетический движущийся источник — отладка без TCC.
		w := opts.Width
		if w <= 0 {
			w = 1280
		}
		args = append(args,
			"-re", // отдавать в реальном темпе, иначе lavfi сыплет кадры быстрее времени
			"-f", "lavfi",
			"-i", fmt.Sprintf("testsrc2=size=%dx720:rate=%d", w, opts.FPS),
		)
	} else {
		args = append(args,
			"-f", "avfoundation",
			"-capture_cursor", "1",
			"-framerate", fmt.Sprintf("%d", opts.FPS),
			"-i", fmt.Sprintf("%d:", opts.ScreenIndex), // "<index>:" = экран, без аудио
		)
		// Даунскейл с Retina — иначе software-VP8 жжёт CPU.
		if opts.Width > 0 {
			args = append(args, "-vf", fmt.Sprintf("scale=%d:-2", opts.Width))
		}
	}

	// Форсируем постоянную частоту кадров (CFR) на выходе. avfoundation
	// отдаёт кадры с переменной частотой (VFR) и реальными таймстемпами;
	// без CFR реальная каденция расходится с фиксированной Duration в
	// WriteSample → RTP-часы уплывают → браузер замирает после 1-го кадра.
	args = append(args,
		"-r", fmt.Sprintf("%d", opts.FPS),
		"-fps_mode", "cfr",
		"-pix_fmt", "yuv420p",
	)

	if opts.Codec == CodecH264 {
		// Аппаратный H264 через VideoToolbox: почти бесплатно по CPU,
		// низкая и стабильная задержка энкода. allow_sw — мягкий фолбэк.
		args = append(args,
			"-c:v", "h264_videotoolbox",
			"-realtime", "1",
			"-allow_sw", "1",
			"-profile:v", "high",
			"-b:v", opts.Bitrate,
			"-g", fmt.Sprintf("%d", opts.FPS),
			// SPS/PPS перед каждым кейфреймом — чтобы новый зритель декодировал.
			"-bsf:v", "dump_extra=freq=keyframe",
			"-f", "h264",
			"-",
		)
	} else {
		// Софтовый VP8 (libvpx, realtime).
		args = append(args,
			"-c:v", "libvpx",
			"-deadline", "realtime",
			"-cpu-used", "8",
			"-lag-in-frames", "0", // без lookahead — убирает скрытую задержку энкодера
			"-threads", fmt.Sprintf("%d", opts.Threads), // 0 = авто (по ядрам)
			"-b:v", opts.Bitrate,
			// кейфрейм раз в секунду: новый зритель быстро получает картинку
			// (компенсация отсутствия PLI-on-demand в прототипе).
			"-g", fmt.Sprintf("%d", opts.FPS),
			"-keyint_min", fmt.Sprintf("%d", opts.FPS),
			"-f", "ivf",
			"-",
		)
	}
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

	// Небольшой буфер: длинная очередь = скрытая задержка. При DropLate
	// держим только свежие кадры, при выключенном — даём backpressure ffmpeg.
	frames := make(chan []byte, 4)
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

		// Парсинг делаем в горутине: чтение заголовка ждёт инициализации
		// ffmpeg и не должно задерживать сигналинг.
		in := bufio.NewReader(stdout)
		if opts.Codec == CodecH264 {
			readH264(ctx, in, frames, opts.DropLate)
		} else {
			readIVF(ctx, in, frames, opts.DropLate)
		}
	}()

	return frames, nil
}

// readIVF читает VP8-кадры из IVF-потока ffmpeg и шлёт их в канал.
func readIVF(ctx context.Context, in io.Reader, frames chan []byte, dropLate bool) {
	reader, _, err := ivfreader.NewWith(in)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("capture: ivf header: %v", err)
		}
		return
	}
	for {
		frame, _, err := reader.ParseNextFrame()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: read frame: %v", err)
			}
			return
		}
		if !pushFrame(ctx, frames, frame, dropLate) {
			return
		}
	}
}

// readH264 читает Annex-B поток H264, группирует NAL-юниты в access unit'ы
// (кадры) и шлёт каждый кадр в канал. Группируем по VCL-границе: при включённом
// realtime VideoToolbox обычно один слайс на кадр, поэтому флашим сразу после
// VCL-NAL — это держит задержку минимальной.
func readH264(ctx context.Context, in io.Reader, frames chan []byte, dropLate bool) {
	reader, err := h264reader.NewReader(in)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("capture: h264 reader: %v", err)
		}
		return
	}
	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	var au []byte
	for {
		nal, err := reader.NextNAL()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("capture: read nal: %v", err)
			}
			return
		}
		au = append(au, startCode...)
		au = append(au, nal.Data...)

		isVCL := nal.UnitType == h264reader.NalUnitTypeCodedSliceNonIdr ||
			nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr
		if isVCL {
			if !pushFrame(ctx, frames, au, dropLate) {
				return
			}
			au = nil
		}
	}
}

// pushFrame отправляет кадр в канал. При dropLate, если буфер полон, выкидывает
// самый старый кадр и кладёт свежий (потребитель всегда видит актуальное);
// иначе — блокирует (backpressure). Возвращает false, если ctx отменён.
func pushFrame(ctx context.Context, frames chan []byte, frame []byte, dropLate bool) bool {
	if dropLate {
		for {
			select {
			case frames <- frame:
				return true
			case <-ctx.Done():
				return false
			default:
				// Буфер полон — выкидываем самый старый кадр и пробуем снова.
				select {
				case <-frames:
				default:
				}
			}
		}
	}
	select {
	case frames <- frame:
		return true
	case <-ctx.Done():
		return false
	}
}

// logStderr построчно льёт stderr ffmpeg в стандартный лог. Шумные строки
// рантайма (objc-предупреждения) и пустые отбрасываем — при -loglevel error
// здесь остаются только реальные ошибки.
func logStderr(r interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "objc[") {
			continue
		}
		log.Printf("ffmpeg: %s", line)
	}
}
