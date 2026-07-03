//go:build linux && cgo

package capture

// Нативный Wayland-захват на GPU: PipeWire (DMABUF) → VAAPI-энкод в ОДНОМ процессе,
// без CPU-копии и raw-пайпа. Компилируется ТОЛЬКО в cgo-сборке под Linux — Mac
// (тег darwin) этот файл и .c не видит вообще.
//
// Шаг A (текущий): подключаемся к PipeWire по fd от портала и логируем, в каком
// формате приходят кадры (DMABUF/память, spa-формат, модификатор, размер). По
// этому формату дальше пишем VAAPI-импорт + h264_vaapi-энкод. Пока откат на gst.

/*
#cgo pkg-config: libpipewire-0.3
#include "native_pw_linux.h"
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"syscall"
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

// nativePipeWireCapture — шаг A: зонд формата кадра. Берём PipeWire fd+node из
// уже работающего портала, ждём кадр, логируем формат, откатываемся на gst.
func nativePipeWireCapture(_ context.Context, _ Options) (chan []byte, error) {
	ps, err := ensurePortal()
	if err != nil {
		return nil, fmt.Errorf("portal: %w", err)
	}
	fd, err := ps.openPipeWire()
	if err != nil {
		return nil, fmt.Errorf("openpipewire: %w", err)
	}
	defer syscall.Close(fd) // C дублирует fd внутри — наш закрываем

	var info C.katana_frame_info
	if rc := C.katana_pw_probe(C.int(fd), C.uint(ps.node), &info); rc != 0 {
		return nil, fmt.Errorf("pw probe: no frame")
	}
	log.Printf("capture: native probe — dmabuf=%d spa_format=%d modifier=0x%x size=%dx%d planes=%d",
		int(info.is_dmabuf), uint(info.spa_format), uint64(info.modifier),
		int(info.width), int(info.height), int(info.n_planes))

	// Шаг A только зондирует — реальный энкод (VAAPI) будет следующим шагом.
	return nil, fmt.Errorf("native probe only (step A)")
}
