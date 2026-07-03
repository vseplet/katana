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

	// Audio — захватывать и передавать звук (system/app audio через SCK → Opus).
	// Работает только для SCK-источников (display/window/app).
	Audio bool

	// Cursor — показывать курсор хоста в захвате. При управлении выключаем,
	// чтобы зритель пользовался мгновенным локальным курсором (без видео-лага).
	Cursor bool

	// DropLate: если потребитель отстаёт, выкидывать старые кадры и держать
	// только свежий, а не копить очередь. Меньше задержка под нагрузкой ценой
	// возможных пропусков кадров.
	DropLate bool

	// TestSource: вместо реального экрана использовать синтетический
	// движущийся testsrc (lavfi). Нужен для отладки без TCC-разрешения
	// на запись экрана — пайплайн энкода/доставки тот же.
	TestSource bool
}

// Stream — результат захвата: каналы видео- и (опционально) аудио-кадров.
// Video — payload IVF-кадра (VP8) или H264 access unit. Audio — Opus-пакеты
// (из ogg), либо nil, если звук не захватывается.
type Stream struct {
	Video <-chan []byte
	Audio <-chan []byte
	// SetCursor меняет видимость курсора хоста в захвате НА ЛЕТУ (без рестарта).
	// nil для источников, где это не поддерживается (avfoundation/тест).
	SetCursor func(show bool)
	// ForceKeyframe просит энкодер выдать keyframe на ближайшем кадре (ответ на
	// PLI зрителя: дропнуть накопленный буфер и догнать live). nil, если путь
	// энкодера это не поддерживает (ffmpeg/VP8/тест).
	ForceKeyframe func()
	// SetBitrate меняет целевой битрейт энкодера на лету, kbps (адаптация к сети).
	// nil, если путь не поддерживает (ffmpeg/VP8/тест).
	SetBitrate func(kbps int)
}

// CaptureEncoder запускает захват+энкод и отдаёт каналы кадров.
//
// Закрытие переданного ctx останавливает захват и завершает subprocess'ы.
// Каналы закрываются, когда поток завершён (ctx отменён или процесс умер).
type CaptureEncoder interface {
	Start(ctx context.Context, opts Options) (*Stream, error)
}
