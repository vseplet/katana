//go:build !darwin

// Заглушки нативной части для не-macOS сборок (headless-linux и т.п.): захвата
// экрана (ScreenCaptureKit), списка источников и инъекции скролла (CGEvent) нет.
// Терминал/сигналинг работают без этого. Реальный Linux-захват (ffmpeg x11grab) —
// отдельный display-билд позже.
package capture

import "context"

// NewEncoder на не-macOS отдаёт энкодер без видео: хост поднимается headless,
// только с терминалом.
func NewEncoder() CaptureEncoder { return noVideoEncoder{} }

type noVideoEncoder struct{}

func (noVideoEncoder) Start(_ context.Context, _ Options) (*Stream, error) {
	ch := make(chan []byte)
	close(ch) // видео недоступно — канал сразу закрыт, писатель завершается
	return &Stream{Video: ch}, nil
}

func ListSources() (Sources, error)             { return Sources{}, nil }
func ActivateApp(_ int) error                   { return nil }
func InjectScroll(_, _ int)                     {}
func SourceRect(_ string, _ int) (Rect, error)  { return Rect{}, nil }
