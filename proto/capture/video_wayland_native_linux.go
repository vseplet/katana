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
	"time"
	"unsafe"
)

// init переключает Wayland-видео на нативный путь в cgo-сборке.
func init() {
	waylandVideoFn = startVideoWaylandNative
}

// nativeFrame — H264 access unit + реальная длительность кадра (таймстамп PipeWire).
// Данные и длительность в ОДНОЙ структуре → не рассинхронятся при дропе.
type nativeFrame struct {
	data []byte
	dur  time.Duration
}

// nativeCh — канал кадров от нативного энкодера (единственный активный захват).
var nativeCh chan nativeFrame

//export goNativeLog
func goNativeLog(msg *C.char) {
	log.Printf("capture: native: %s", C.GoString(msg))
}

var nativeDrops, nativeTotal int

//export goNativeH264
func goNativeH264(data unsafe.Pointer, n C.int, durNs C.longlong) {
	ch := nativeCh
	if ch == nil {
		return
	}
	f := nativeFrame{data: C.GoBytes(data, n), dur: time.Duration(durNs)}
	nativeTotal++
	select {
	case ch <- f:
	default: // канал полон — дропаем (иначе блокируем C-поток); P-кадр теряется
		nativeDrops++
	}
	if nativeTotal%300 == 0 && nativeDrops > 0 {
		log.Printf("capture: native dropped %d/%d h264 frames (channel full)", nativeDrops, nativeTotal)
	}
}

// startVideoWaylandNative — нативный захват; при ошибке инициализации откат на gst.
func startVideoWaylandNative(ctx context.Context, opts Options) (chan []byte, chan time.Duration, error) {
	video, dur, err := nativePipeWireCapture(ctx, opts)
	if err != nil {
		log.Printf("capture: native wayland (%v) — fallback to gst", err)
		return startVideoWaylandGst(ctx, opts)
	}
	return video, dur, nil
}

func nativePipeWireCapture(ctx context.Context, opts Options) (chan []byte, chan time.Duration, error) {
	ps, err := ensurePortal()
	if err != nil {
		return nil, nil, fmt.Errorf("portal: %w", err)
	}
	fd, err := ps.openPipeWire()
	if err != nil {
		return nil, nil, fmt.Errorf("openpipewire: %w", err)
	}
	fps := opts.FPS
	if fps <= 0 {
		fps = 30
	}
	kbps := bitrateKbps(opts.Bitrate)

	// Целевой размер — из настройки Width зрителя (даунскейл на GPU через
	// scale_vaapi). 0 = как в потоке (без даунскейла).
	tw, th := 0, 0
	if opts.Width > 0 {
		tw = opts.Width
		if sw, sh := ScreenSize(); sw > 0 && sh > 0 {
			th = opts.Width * sh / sw
		} else {
			th = opts.Width * 9 / 16
		}
		tw -= tw % 2
		th -= th % 2
	}

	frames := make(chan nativeFrame, 8)
	nativeCh = frames
	// Публичные каналы: сплиттер разводит nativeFrame на Video и VideoDur строго
	// в лок-степе (по одному элементу на кадр), потому рассинхрона не бывает.
	videoOut := make(chan []byte, 8)
	durOut := make(chan time.Duration, 8)
	go func() {
		for f := range frames {
			videoOut <- f.data
			durOut <- f.dur
		}
		close(videoOut)
		close(durOut)
	}()
	cfg := C.katana_native_cfg{
		fd:     C.int(fd),
		node:   C.uint(ps.node),
		width:  C.int(tw),
		height: C.int(th),
		fps:    C.int(fps),
		kbps:   C.int(kbps),
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
	return videoOut, durOut, nil
}
