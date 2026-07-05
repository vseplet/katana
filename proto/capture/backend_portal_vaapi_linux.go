//go:build linux && cgo

package capture

// Бэкенд захвата: xdg-desktop-portal (ScreenCast) + PipeWire → libav VAAPI → H264,
// zero-copy через dmabuf. ЦЕЛЕВОЙ СТЕК: Wayland + KDE/KWin (или GNOME) портал +
// AMD GPU (фильтр DCC-модификаторов в .c — AMD-специфика; Intel вероятно ОК,
// NVIDIA — нет). Компилируется ТОЛЬКО в cgo-сборке под Linux; Mac (тег darwin)
// этот файл и .c не видит. C-часть — backend_portal_vaapi_linux.c. Матрица
// поддержки целиком — в doc.go.

/*
#cgo pkg-config: libpipewire-0.3 libavcodec libavfilter libavutil libva egl gbm libdrm
#include "backend_portal_vaapi_linux.h"
*/
import "C"

import (
	"context"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// dumpFile — если задан env KATANA_H264_DUMP, дублируем сюда сырой Annex-B H264
// (тот же поток, что уходит в WebRTC) для диагностики «без WebRTC»: играем файл
// локально и смотрим, есть ли прыжки кадров ДО энкодера/сети.
var dumpFile *os.File

func init() {
	if p := os.Getenv("KATANA_H264_DUMP"); p != "" {
		if f, err := os.Create(p); err == nil {
			dumpFile = f
			log.Printf("capture: native: H264 dump -> %s", p)
		}
	}
}

// init переключает Wayland-видео на нативный путь в cgo-сборке.
func init() {
	waylandVideoFn = startVideoWaylandNative
}

// nativeCh — канал H264 access unit'ов от нативного энкодера (единственный захват).
var nativeCh chan []byte

//export goNativeLog
func goNativeLog(msg *C.char) {
	log.Printf("capture: native: %s", C.GoString(msg))
}

var nativeDrops, nativeTotal int

//export goNativeH264
func goNativeH264(data unsafe.Pointer, n C.int) {
	ch := nativeCh
	if ch == nil {
		return
	}
	b := C.GoBytes(data, n)
	if dumpFile != nil {
		dumpFile.Write(b)
	}
	nativeTotal++
	select {
	case ch <- b:
	default: // канал полон — дропаем (иначе блокируем C-поток); P-кадр теряется
		nativeDrops++
	}
	if nativeTotal%300 == 0 && nativeDrops > 0 {
		log.Printf("capture: native dropped %d/%d h264 frames (channel full)", nativeDrops, nativeTotal)
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

	// CFR-таймер в C гонит кадры ровно на fps (как Mac) → отдаём обычным байтовым
	// каналом с фиксированной длительностью в WriteSample. Метки времени не нужны.
	frames := make(chan []byte, 8)
	nativeCh = frames
	cfg := C.katana_native_cfg{
		fd:     C.int(fd),
		node:   C.uint(ps.node),
		width:  C.int(tw),
		height: C.int(th),
		fps:    C.int(fps),
		kbps:   C.int(kbps),
	}
	// ГОНКА СТАРТ/СТОП: katana_native_stop() лишь сбрасывает флаг, а start ставит
	// его заново уже ПОСЛЕ — stop, прилетевший во время инициализации C (портал/
	// PipeWire), терялся, и захват-сирота крутился вечно (а stop() стримера ждал
	// его навсегда → хост не переподключался к брокеру). Поэтому: (1) с отменённым
	// ctx вообще не стартуем; (2) стоп ПОВТОРЯЕМ, пока start реально не вернётся.
	if ctx.Err() != nil {
		syscall.Close(fd)
		return nil, ctx.Err()
	}
	startDone := make(chan struct{})
	go func() {
		C.katana_native_start(cfg) // блокирует до katana_native_stop
		close(startDone)
		syscall.Close(fd)
		close(frames)
		nativeCh = nil
		waylandForceKey, waylandSetBitrate = nil, nil
		log.Printf("capture stopped (native)")
	}()
	go func() {
		<-ctx.Done()
		for {
			C.katana_native_stop()
			select {
			case <-startDone:
				return
			case <-time.After(200 * time.Millisecond): // стоп мог опередить старт — добиваем
			}
		}
	}()
	// Контур обратной связи WebRTC: PLI зрителя → мгновенный IDR; адаптация
	// битрейта к сети → переоткрытие энкодера на лету.
	waylandForceKey = func() { C.katana_native_force_key() }
	waylandSetBitrate = func(kbps int) { C.katana_native_set_bitrate(C.int(kbps)) }
	log.Printf("capture: native wayland (pipewire→vaapi, %dfps %dk)", fps, kbps)
	return frames, nil
}
