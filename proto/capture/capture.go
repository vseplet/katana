package capture

import "context"

// Options описывает параметры захвата+энкода экрана.
type Options struct {
	ScreenIndex int    // индекс avfoundation-устройства экрана (см. -list_devices)
	Width       int    // целевая ширина в пикселях; 0 = нативное разрешение
	FPS         int    // частота кадров
	Bitrate     string // целевой битрейт, напр. "3M"

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
