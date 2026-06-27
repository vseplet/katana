package capture

import "context"

// Codec — кодек видео. Определяет энкодер ffmpeg, формат вывода и MimeType
// WebRTC-трека.
type Codec string

const (
	CodecVP8  Codec = "vp8"  // libvpx, софтовый
	CodecH264 Codec = "h264" // h264_videotoolbox, аппаратный (Apple)
)

// Options описывает параметры захвата+энкода экрана.
type Options struct {
	// SourceKind — что захватываем: "screen" (avfoundation, весь экран по
	// ScreenIndex), "window" или "app" (ScreenCaptureKit по SourceID).
	SourceKind string
	SourceID   int // windowID (для window) или pid (для app)

	ScreenIndex int    // индекс avfoundation-устройства экрана (см. -list_devices)
	Codec       Codec  // vp8 | h264
	Width       int    // целевая ширина в пикселях; 0 = нативное разрешение
	FPS         int    // частота кадров
	Bitrate     string // целевой битрейт, напр. "3M"

	// Threads — число потоков энкодера ffmpeg (-threads). 0 = авто (по ядрам).
	// Многопоточный софт-VP8 быстрее кодирует кадр → меньше задержка энкода.
	Threads int

	// DropLate: если потребитель отстаёт, выкидывать старые кадры и держать
	// только свежий, а не копить очередь. Меньше задержка под нагрузкой ценой
	// возможных пропусков кадров.
	DropLate bool

	// TestSource: вместо реального экрана использовать синтетический
	// движущийся testsrc (lavfi). Нужен для отладки без TCC-разрешения
	// на запись экрана — пайплайн энкода/доставки тот же.
	TestSource bool
}

// CaptureEncoder запускает захват+энкод экрана и отдаёт поток VP8-кадров.
//
// Каждый элемент канала — payload одного IVF-кадра (готовый VP8-кадр),
// который можно напрямую отдать в track.WriteSample.
//
// Закрытие переданного ctx останавливает захват и завершает subprocess.
// Канал закрывается, когда поток завершён (ctx отменён или процесс умер).
type CaptureEncoder interface {
	Start(ctx context.Context, opts Options) (<-chan []byte, error)
}
