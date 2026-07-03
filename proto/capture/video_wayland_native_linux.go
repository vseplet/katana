//go:build linux && cgo

package capture

// Нативный Wayland-захват на GPU: PipeWire (DMABUF) → VAAPI-энкод в ОДНОМ процессе,
// без CPU-копии и raw-пайпа (в отличие от gst→ffmpeg-фолбэка). Компилируется
// ТОЛЬКО в cgo-сборке под Linux — Mac (тег darwin) этот файл не видит вообще.
//
// Требует -dev заголовки libpipewire/libva/libavcodec при СБОРКЕ (на SteamOS —
// через distrobox, см. steamos.sh). Пока — каркас: реальная C-часть
// (nativePipeWireCapture) ещё не реализована, поэтому откатываемся на gst, чтобы
// видео не пропадало во время разработки.

import (
	"context"
	"fmt"
	"log"
)

// init переключает Wayland-видео на нативный путь в cgo-сборке.
func init() {
	waylandVideoFn = startVideoWaylandNative
}

// startVideoWaylandNative — нативный захват; при ошибке/неготовности откат на gst.
func startVideoWaylandNative(ctx context.Context, opts Options) (chan []byte, error) {
	ch, err := nativePipeWireCapture(ctx, opts)
	if err != nil {
		log.Printf("capture: native wayland (%v) — fallback to gst", err)
		return startVideoWaylandGst(ctx, opts)
	}
	return ch, nil
}

// nativePipeWireCapture — заглушка. Реальная реализация (cgo: libpipewire для
// DMABUF-кадров + libva/libavcodec h264_vaapi для энкода на GPU) добавляется
// следующими шагами, компилируется/отлаживается в distrobox на Deck.
func nativePipeWireCapture(_ context.Context, _ Options) (chan []byte, error) {
	return nil, fmt.Errorf("native pipewire capture not yet implemented")
}
