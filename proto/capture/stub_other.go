//go:build !darwin && !linux

// Заглушки нативной части для платформ без захвата (Windows/BSD и т.п.): захвата
// экрана, списка источников и инъекции скролла нет. Терминал/сигналинг работают
// без этого. macOS — ScreenCaptureKit/ffmpeg, Linux — ffmpeg x11grab/pulse
// (ffmpeg_linux.go); тип noVideoEncoder общий (ffmpeg_common.go).
package capture

// NewEncoder на платформах без захвата отдаёт энкодер без видео: хост поднимается
// headless, только с терминалом.
func NewEncoder() CaptureEncoder { return noVideoEncoder{} }

func ListSources() (Sources, error)            { return Sources{}, nil }
func ActivateApp(_ int) error                  { return nil }
func InjectScroll(_, _ int)                    {}
func SourceRect(_ string, _ int) (Rect, error) { return Rect{}, nil }
