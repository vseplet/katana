//go:build windows

package capture

// Захват на Windows, общая часть: разрешение экрана (GetSystemMetrics), список
// источников (мониторы + top-level окна для per-window WGC), выбор энкодера.
// Видео — Windows.Graphics.Capture (WGC) → Media Foundation H264 Encoder MFT,
// нативно, без ffmpeg и без cgo (wgc_windows.go, mf_h264_windows.go, com_windows.go,
// d3d11_windows.go). Звук — системный loopback через WASAPI → ffmpeg → Opus
// (audio_wasapi_windows.go); если ffmpeg нет — деградируем до без-аудио, как Linux.
//
// Все COM/WinRT-вызовы идут через syscall (LazyDLL + vtable). Если рантайм не
// поднимает нужный стек (нет WGC/энкодера — напр. урезанный ARM-образ или VM без
// поддержки), VideoAvailable() вернёт false и хост поднимется headless.

import (
	"context"
	"log"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procGetSystemMetric = user32.NewProc("GetSystemMetrics")
	procEnumWindows     = user32.NewProc("EnumWindows")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
	procGetWindowRect   = user32.NewProc("GetWindowRect")
	procSetForeground   = user32.NewProc("SetForegroundWindow")
)

const (
	smCXScreen = 0
	smCYScreen = 1
)

// IsWaylandSession на Windows всегда false (термин из Linux; здесь для единого API
// пакета — часть кода main спрашивает его платформо-нейтрально).
func IsWaylandSession() bool { return false }

// ScreenSize — разрешение основного дисплея (пиксели). GetSystemMetrics отдаёт
// размер первичного монитора; 0,0 если не удалось.
func ScreenSize() (int, int) {
	w, _, _ := procGetSystemMetric.Call(smCXScreen)
	h, _, _ := procGetSystemMetric.Call(smCYScreen)
	return int(w), int(h)
}

// SourceRect — прямоугольник источника. Для экрана — весь первичный дисплей; для
// окна — его экранные координаты (GetWindowRect по HWND=SourceID). Нужен для
// маппинга нормализованных координат мыши зрителя в пиксели.
func SourceRect(kind string, id int) (Rect, error) {
	if kind == "window" && id != 0 {
		r := new(struct{ left, top, right, bottom int32 }) // out на куче
		ok, _, _ := procGetWindowRect.Call(uintptr(id), uintptr(unsafe.Pointer(r)))
		runtime.KeepAlive(r)
		if ok != 0 {
			return Rect{X: float64(r.left), Y: float64(r.top),
				W: float64(r.right - r.left), H: float64(r.bottom - r.top)}, nil
		}
	}
	w, h := ScreenSize()
	if w <= 0 || h <= 0 {
		return Rect{}, nil
	}
	return Rect{X: 0, Y: 0, W: float64(w), H: float64(h)}, nil
}

// ListSources — первичный дисплей + видимые top-level окна (для per-window WGC).
// ID окна — это HWND (используется как SourceID в Options при захвате окна).
func ListSources() (Sources, error) {
	var s Sources
	if w, h := ScreenSize(); w > 0 && h > 0 {
		s.Displays = append(s.Displays, SourceDisplay{ID: 0, Width: w, Height: h})
	}
	// EnumWindows с Go-колбэком (syscall.NewCallback). Берём только видимые окна
	// с непустым заголовком.
	cb := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if vis, _, _ := procIsWindowVisible.Call(hwnd); vis == 0 {
			return 1 // продолжаем перечисление
		}
		buf := make([]uint16, 256)
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return 1
		}
		title := windows.UTF16ToString(buf[:n])
		s.Windows = append(s.Windows, SourceWindow{ID: int(hwnd), Title: title})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return s, nil
}

// ActivateApp выводит окно на передний план (SourceID = HWND). На Windows «app» и
// «window» ведём к одному действию.
func ActivateApp(id int) error {
	if id != 0 {
		procSetForeground.Call(uintptr(id))
	}
	return nil
}

// InjectScroll на Windows не нужен — скролл идёт через ввод (SendInput, input_windows.go).
func InjectScroll(_, _ int) {}

// AudioAvailable — доступен ли системный звук: WASAPI loopback есть всегда
// (Win10+), но Opus кодирует ffmpeg (нативного Opus-энкодера на Windows нет),
// поэтому гейтим по наличию ffmpeg. Нет ffmpeg → без-аудио.
func AudioAvailable() bool { return FFmpegPath() != "" }

// VideoAvailable — доступен ли нативный видео-путь (WGC + H264 MFT). Проверяем
// рантайм-пробой: поднимается ли D3D11-девайс и создаётся ли H264-энкодер MFT.
// Кэшируется в videoProbe().
func VideoAvailable() bool { return videoProbe() }

// NewEncoder на Windows: нативный WGC+MFT-энкодер, если доступно видео; иначе
// headless-заглушка (только терминал/сигналинг).
func NewEncoder() CaptureEncoder {
	if !VideoAvailable() {
		return noVideoEncoder{}
	}
	return &captureWindows{}
}

// captureWindows — Windows-энкодер: WGC-захват → Media Foundation H264 MFT.
type captureWindows struct{}

// Start поднимает захват+энкод и возвращает каналы кадров (H264 access unit'ы).
func (c *captureWindows) Start(ctx context.Context, opts Options) (*Stream, error) {
	// Осиротевшие ffmpeg от прошлых запусков держат AMF-сессию AMD → аппаратный
	// энкодер падает на инициализации. Чистим ДО старта своих ffmpeg (видео/звук
	// поднимаются ниже), иначе убили бы свои же.
	killStrayFFmpeg()
	video, ctl, err := startVideoWGC(ctx, opts)
	if err != nil {
		log.Printf("capture: video (wgc/mf): %v (continuing without video)", err)
		ch := make(chan []byte)
		close(ch)
		return &Stream{Video: ch}, nil
	}
	st := &Stream{Video: video}
	if ctl != nil {
		st.ForceKeyframe = ctl.forceKeyframe
		st.SetBitrate = ctl.setBitrate
		st.SetCursor = ctl.setCursor
	}
	// Звук (WASAPI loopback → ffmpeg → Opus), если есть ffmpeg. Ошибка — не
	// фатально: продолжаем без аудио (как раньше).
	if AudioAvailable() {
		if audio, aerr := startAudioWindows(ctx); aerr != nil {
			log.Printf("capture: audio (wasapi/ffmpeg): %v (continuing without audio)", aerr)
		} else {
			st.Audio = audio
		}
	}
	return st, nil
}
