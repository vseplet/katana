package capture

// Описания захватываемых источников и геометрии — чистые данные (JSON), общие для
// всех платформ. Наполняет их ListSources (на macOS — из ScreenCaptureKit; на
// других платформах — заглушка). Держим здесь, а не в *_darwin, чтобы ядро и
// не-macOS сборки видели типы.

// SourceDisplay — дисплей (захват всего экрана).
type SourceDisplay struct {
	ID     int `json:"id"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// SourceWindow — отдельное окно.
type SourceWindow struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	App    string `json:"app"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// SourceApp — запущенное приложение (захват всех его окон).
type SourceApp struct {
	PID      int    `json:"pid"`
	BundleID string `json:"bundleId"`
	Name     string `json:"name"`
}

// Sources — всё, что можно захватить.
type Sources struct {
	Displays []SourceDisplay `json:"displays"`
	Windows  []SourceWindow  `json:"windows"`
	Apps     []SourceApp     `json:"apps"`
}

// Rect — глобальный прямоугольник источника (точки, top-left).
type Rect struct {
	X, Y, W, H float64
}
