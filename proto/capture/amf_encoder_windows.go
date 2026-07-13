//go:build windows && cgo && winnative

// Go-биндинг к нативному AMF-энкодеру (amf_encoder_windows.c). Тонкая обёртка: как и
// у MF-энкодера, C отдаёт готовые Annex-B access unit'ы, здесь мы кешируем SPS/PPS и
// подставляем их перед IDR, затем гоним кадры в frames. Реализует winVideoEncoder,
// поэтому подставляется вместо MF-энкодера в native_bridge_winnative.go по гейту.

package capture

/*
#cgo CFLAGS: -I${SRCDIR}/../thirdparty/amf -D_WIN32_WINNT=0x0A00 -DCOBJMACROS
#cgo LDFLAGS: -ld3d11 -ldxgi -ldxguid -lole32 -loleaut32 -luuid
#include <stdlib.h>
#include "amf_encoder_windows.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"
)

// amfAvailable — есть ли рантайм AMF (для гейта в native_bridge_winnative.go).
func amfAvailable() bool { return C.katana_amf_available() != 0 }

type amfEncoder struct {
	h      *C.katana_amf
	mu     sync.Mutex
	done   chan struct{}
	ctx    context.Context
	frames chan []byte
	sps    []byte // с 4-байтовым старт-кодом
	pps    []byte
}

// newAMFEncoder поднимает AMF AVC-энкодер на D3D11-девайсе WGC-захвата и запускает
// poll-горутину. Ошибка → gate откатывается на MF-энкодер.
func newAMFEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int) (*amfEncoder, error) {
	var hr C.int32_t
	var stage C.int
	var info [256]C.char
	handle := C.katana_amf_create(unsafe.Pointer(dev), C.int(w), C.int(h),
		C.int(fps), C.int(kbps), C.int(gop), &hr, &stage, &info[0], C.int(len(info)))
	if handle == nil {
		return nil, fmt.Errorf("AMF encoder create: stage=%d res=%d", int(stage), int(hr))
	}
	log.Printf("capture: AMF encoder active — %s", C.GoString(&info[0]))

	e := &amfEncoder{h: handle, done: make(chan struct{}), ctx: ctx, frames: frames}
	e.primeHeaders()
	go e.pollLoop()
	return e, nil
}

// primeHeaders забирает SPS/PPS из ExtraData энкодера и раскладывает в кеш — AMF не
// вставляет заголовки инлайн в кадры, поэтому withHeaders подставит их перед каждым IDR.
func (e *amfEncoder) primeHeaders() {
	var buf [512]byte
	n := int(C.katana_amf_extradata(e.h, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	if n <= 0 {
		return
	}
	forEachNAL(buf[:n], func(t byte, nal []byte) {
		switch t {
		case 7:
			e.sps = prependStartCode(nal)
		case 8:
			e.pps = prependStartCode(nal)
		}
	})
	if e.sps != nil && e.pps != nil {
		log.Printf("capture: AMF SPS/PPS primed (%d+%d байт)", len(e.sps), len(e.pps))
	}
}

func (e *amfEncoder) pollLoop() {
	buf := make([]byte, 1<<20)
	logged := false
	var winBytes, winAUs, winSlices int
	winStart := time.Now()
	lastAU := time.Now()
	stalled := false
	for {
		select {
		case <-e.done:
			return
		case <-e.ctx.Done():
			return
		default:
		}
		e.mu.Lock()
		if e.h == nil {
			e.mu.Unlock()
			return
		}
		n := int(C.katana_amf_poll(e.h, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
		e.mu.Unlock()
		if n == -2 {
			buf = make([]byte, len(buf)*2)
			continue
		}
		if n <= 0 {
			if !stalled && time.Since(lastAU) > 400*time.Millisecond {
				stalled = true
				log.Printf("capture: AMF output STALLED — нет AU уже %.0fмс", time.Since(lastAU).Seconds()*1000)
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if stalled {
			log.Printf("capture: AMF output resumed после простоя %.0fмс", time.Since(lastAU).Seconds()*1000)
			stalled = false
		}
		lastAU = time.Now()
		if !logged {
			logged = true
			m := n
			if m > 8 {
				m = 8
			}
			log.Printf("capture: AMF first AU %d bytes, head=% x", n, buf[:m])
		}
		winBytes += n
		winAUs++
		winSlices += countVCLNALs(buf[:n]) // проверка реальной нарезки на слайсы
		if el := time.Since(winStart); el >= 2*time.Second {
			log.Printf("capture: AMF out %.0f kbps, %.0f AU/s (avg %d B/AU, %.1f slices/frame)",
				float64(winBytes)*8/1000/el.Seconds(), float64(winAUs)/el.Seconds(),
				winBytes/(winAUs+1), float64(winSlices)/float64(winAUs+1))
			winBytes, winAUs, winSlices, winStart = 0, 0, 0, time.Now()
		}
		au := e.withHeaders(buf[:n])
		if !pushFrame(e.ctx, e.frames, au, true) {
			return
		}
	}
}

// withHeaders подставляет кешированные SPS/PPS перед IDR, где энкодер их не включил.
func (e *amfEncoder) withHeaders(au []byte) []byte {
	var hasSPS, hasPPS, hasIDR bool
	forEachNAL(au, func(t byte, nal []byte) {
		switch t {
		case 7:
			e.sps = prependStartCode(nal)
			hasSPS = true
		case 8:
			e.pps = prependStartCode(nal)
			hasPPS = true
		case 5:
			hasIDR = true
		}
	})
	if hasIDR && !(hasSPS && hasPPS) && e.sps != nil && e.pps != nil {
		out := make([]byte, 0, len(e.sps)+len(e.pps)+len(au))
		out = append(out, e.sps...)
		out = append(out, e.pps...)
		out = append(out, au...)
		return out
	}
	return append([]byte(nil), au...)
}

func (e *amfEncoder) initVProc(srcW, srcH int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return false
	}
	var hr C.int32_t
	rc := C.katana_amf_init_vproc(e.h, C.int(srcW), C.int(srcH), &hr)
	if rc != 0 {
		log.Printf("capture: AMF zero-copy VP unavailable (hr=0x%08x) — CPU NV12 path", uint32(hr))
		return false
	}
	log.Printf("capture: AMF zero-copy active — GPU BGRA→NV12 (VideoProcessor)")
	return true
}

func (e *amfEncoder) captureTexture(bgraTex uintptr) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("AMF encoder closed")
	}
	if rc := C.katana_amf_capture_texture(e.h, unsafe.Pointer(bgraTex)); rc < 0 {
		return fmt.Errorf("AMF capture_texture: %d", int(rc))
	}
	return nil
}

func (e *amfEncoder) encodeCaptured() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("AMF encoder closed")
	}
	if rc := C.katana_amf_encode_captured(e.h); rc < 0 {
		return fmt.Errorf("AMF encode_captured: %d", int(rc))
	}
	return nil
}

func (e *amfEncoder) submit(nv12 []byte) error {
	if len(nv12) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("AMF encoder closed")
	}
	if rc := C.katana_amf_submit(e.h, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.int(len(nv12))); rc < 0 {
		return fmt.Errorf("AMF submit: %d", int(rc))
	}
	return nil
}

// lossLocalized — AMF режет кадр на слайсы + intra-refresh → потеря локализуется,
// кейфрейм-восстановитель компактнее. Разрешает более короткий дебаунс PLI.
func (e *amfEncoder) lossLocalized() bool { return true }

func (e *amfEncoder) forceKeyframe() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h != nil {
		C.katana_amf_force_keyframe(e.h)
	}
}

func (e *amfEncoder) setBitrate(kbps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h != nil {
		C.katana_amf_set_bitrate(e.h, C.int(kbps))
	}
}

func (e *amfEncoder) Close() {
	select {
	case <-e.done:
	default:
		close(e.done)
	}
	e.mu.Lock()
	if e.h != nil {
		C.katana_amf_destroy(e.h)
		e.h = nil
	}
	e.mu.Unlock()
}
