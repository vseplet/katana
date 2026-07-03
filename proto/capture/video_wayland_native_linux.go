//go:build linux && cgo

package capture

// Нативный Wayland-захват на GPU: PipeWire → libav (VAAPI) → H264 в одном
// процессе, без gst и raw-пайпа. Компилируется ТОЛЬКО в cgo-сборке под Linux —
// Mac (тег darwin) этот файл и .c не видит вообще. C-часть — native_pw_linux.c.

/*
#cgo pkg-config: libpipewire-0.3 libavcodec libavfilter libavutil
#include "native_pw_linux.h"
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"syscall"
	"unsafe"
)

// init переключает Wayland-видео на нативный путь в cgo-сборке.
func init() {
	waylandVideoFn = startVideoWaylandNative
}

// nativeCh — канал H264 access unit'ов от нативного энкодера (единственный захват).
var nativeCh chan []byte

//export goNativeH264
func goNativeH264(data unsafe.Pointer, n C.int) {
	ch := nativeCh
	if ch == nil {
		return
	}
	b := C.GoBytes(data, n)
	select {
	case ch <- b:
	default: // канал полон — дропаем, чтобы не блокировать C-поток PipeWire
	}
}

// startVideoWaylandNative — нативный захват; при ошибке инициализации откат на gst.
func startVideoWaylandNative(ctx context.Context, opts Options) (chan []byte, error) {
	ch, err := nativePipeWireCapture(ctx, opts)
	if err != nil {
		log.Printf("capture: native wayland (%v) — fallback to gst", err)
		return startVideoWaylandGst(ctx, opts)
	}
	return ch, nil
}

func nativePipeWireCapture(ctx context.Context, opts Options) (chan []byte, error) {
	ps, err := ensurePortal()
	if err != nil {
		return nil, fmt.Errorf("portal: %w", err)
	}
	fd, err := ps.openPipeWire()
	if err != nil {
		return nil, fmt.Errorf("openpipewire: %w", err)
	}
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	kbps := bitrateKbps(opts.Bitrate)

	frames := make(chan []byte, 8)
	nativeCh = frames
	cfg := C.katana_native_cfg{
		fd:   C.int(fd),
		node: C.uint(ps.node),
		fps:  C.int(fps),
		kbps: C.int(kbps),
	}
	go func() {
		C.katana_native_start(cfg) // блокирует до katana_native_stop
		syscall.Close(fd)
		close(frames)
		nativeCh = nil
		log.Printf("capture stopped (native)")
	}()
	go func() {
		<-ctx.Done()
		C.katana_native_stop()
	}()
	log.Printf("capture: native wayland (pipewire→vaapi, %dfps %dk)", fps, kbps)
	return frames, nil
}
