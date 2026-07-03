package capture

// Платформо-нейтральная часть ffmpeg-пути: чтение выходных потоков ffmpeg
// (IVF/VP8 и Annex-B/H264), backpressure/drop и лог stderr. Общая для macOS
// (ffmpeg_darwin.go, screencapturekit_darwin.go) и Linux (ffmpeg_linux.go).

import (
	"bufio"
	"context"
	"io"
	"log"
	"strings"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

// noVideoEncoder — headless-заглушка: видео недоступно (нет графики), канал сразу
// закрыт, писатель завершается. Терминал/сигналинг работают без видео. Общая для
// не-macOS сборок без захвата (Windows/BSD — stub_other.go) и Linux без $DISPLAY
// (ffmpeg_linux.go: NewEncoder отдаёт её, когда графики нет).
type noVideoEncoder struct{}

func (noVideoEncoder) Start(_ context.Context, _ Options) (*Stream, error) {
	ch := make(chan []byte)
	close(ch) // видео недоступно — канал сразу закрыт
	return &Stream{Video: ch}, nil
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
// (кадры) и шлёт каждый кадр в канал. Группируем по VCL-границе: обычно один
// слайс на кадр, поэтому флашим сразу после VCL-NAL — задержка минимальна.
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
