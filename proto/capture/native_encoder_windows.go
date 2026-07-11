//go:build windows && cgo && winnative

// Нативный in-process аппаратный H264-энкодер (Media Foundation, async hardware MFT),
// делящий D3D11-устройство с WGC-захватом — как OBS/Sunshine. Реализация COM на C в
// mf_encoder_windows.c; здесь — тонкие Go-биндинги. Собирается только под тегом
// winnative (cgo на виндовом раннере); обычный релиз (CGO_ENABLED=0) живёт на ffmpeg.

package capture

/*
#cgo CFLAGS: -D_WIN32_WINNT=0x0A00 -DCOBJMACROS
#cgo LDFLAGS: -lmfplat -lmfuuid -lmf -lmfreadwrite -lstrmiids -lwmcodecdspuuid -ld3d11 -ldxgi -ldxguid -lole32 -loleaut32 -luuid

#include <stdlib.h>
#include "mf_encoder_windows.h"
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

// nativeEncoder — обёртка над katana_enc (C). C-энкодер отдаёт уже готовые целые
// Annex-B access unit'ы (по одному на poll), поэтому потоковый h264reader не нужен:
// повтор SPS/PPS перед IDR делаем по-AU прямо здесь (без пайпа и лишней латенси),
// затем кладём кадр в frames напрямую с дропом устаревших (realtime-экран).
type nativeEncoder struct {
	h      *C.katana_enc
	mu     sync.Mutex // сериализует доступ к h между poll-горутиной, submit и Close
	done   chan struct{}
	ctx    context.Context
	frames chan []byte
	sps    []byte // закешированы с 4-байтовым старт-кодом
	pps    []byte
}

// newNativeEncoder поднимает аппаратный H264-MFT на D3D11-устройстве WGC-захвата и
// запускает poll-горутину, которая гонит готовые AU в frames.
func newNativeEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int, dropLate bool) (*nativeEncoder, error) {
	_ = dropLate // realtime-экран: всегда дропаем устаревшие кадры (см. pollLoop)
	var hr C.int32_t
	var stage C.int
	var info [256]C.char
	handle := C.katana_enc_create(unsafe.Pointer(dev), C.int(w), C.int(h),
		C.int(fps), C.int(kbps), C.int(gop), &hr, &stage, &info[0], C.int(len(info)))
	if handle == nil {
		return nil, fmt.Errorf("native H264 MFT create: stage=%d hr=0x%08x [%s]",
			int(stage), uint32(hr), C.GoString(&info[0]))
	}
	log.Printf("capture: native MFT active — %s", C.GoString(&info[0]))

	e := &nativeEncoder{h: handle, done: make(chan struct{}), ctx: ctx, frames: frames}
	go e.pollLoop()
	return e, nil
}

// pollLoop тянет готовые AU из C, чинит заголовки и кладёт в frames.
func (e *nativeEncoder) pollLoop() {
	buf := make([]byte, 1<<20) // 1 MiB под один AU; растёт при -2
	logged := false
	// Диагностика реального битрейта/частоты вывода — держит ли MFT CBR.
	var winBytes, winAUs int
	winStart := time.Now()
	// Вахтенный на выход: ловим САМ момент затыка (0 fps), а не усреднение раз в 2с.
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
		n := int(C.katana_enc_poll(e.h, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
		e.mu.Unlock()
		if n == -2 { // не влез — увеличиваем буфер и пробуем снова
			buf = make([]byte, len(buf)*2)
			continue
		}
		if n <= 0 {
			if !stalled && time.Since(lastAU) > 400*time.Millisecond {
				stalled = true
				log.Printf("capture: native output STALLED — нет AU уже %.0fмс (энкодер встал)",
					time.Since(lastAU).Seconds()*1000)
			}
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if stalled {
			log.Printf("capture: native output resumed после простоя %.0fмс",
				time.Since(lastAU).Seconds()*1000)
			stalled = false
		}
		lastAU = time.Now()
		if !logged {
			logged = true
			m := n
			if m > 8 {
				m = 8
			}
			log.Printf("capture: native first AU %d bytes, head=% x", n, buf[:m])
		}
		winBytes += n
		winAUs++
		if el := time.Since(winStart); el >= 2*time.Second {
			log.Printf("capture: native out %.0f kbps, %.0f AU/s (avg %d B/AU)",
				float64(winBytes)*8/1000/el.Seconds(), float64(winAUs)/el.Seconds(),
				winBytes/(winAUs+1))
			winBytes, winAUs, winStart = 0, 0, time.Now()
		}
		au := e.withHeaders(buf[:n]) // свежий срез, безопасно отдавать в канал
		if !pushFrame(e.ctx, e.frames, au, true) {
			return
		}
	}
}

// withHeaders кеширует SPS/PPS и подставляет их перед IDR, где энкодер их не включил
// (зритель заходит в середину потока / потерял пакет — без заголовков декодер не
// заведётся). Всегда возвращает свежевыделенный срез: buf переиспользуется poll-циклом.
func (e *nativeEncoder) withHeaders(au []byte) []byte {
	var hasSPS, hasPPS, hasIDR bool
	forEachNAL(au, func(t byte, nal []byte) {
		switch t {
		case 7: // SPS
			e.sps = prependStartCode(nal)
			hasSPS = true
		case 8: // PPS
			e.pps = prependStartCode(nal)
			hasPPS = true
		case 5: // IDR slice
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

// prependStartCode копирует NAL со свежим 4-байтовым старт-кодом (для кеша SPS/PPS).
func prependStartCode(nal []byte) []byte {
	out := make([]byte, 4+len(nal))
	out[3] = 1
	copy(out[4:], nal)
	return out
}

// forEachNAL разбирает Annex-B (старт-коды 00 00 01 или 00 00 00 01) и вызывает fn на
// каждый NAL: тип (nal[0]&0x1f) и тело NAL без старт-кода.
func forEachNAL(b []byte, fn func(t byte, nal []byte)) {
	type mark struct{ pos, length int }
	var starts []mark
	i := 0
	for i+3 <= len(b) {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				starts = append(starts, mark{i, 3})
				i += 3
				continue
			}
			if i+4 <= len(b) && b[i+2] == 0 && b[i+3] == 1 {
				starts = append(starts, mark{i, 4})
				i += 4
				continue
			}
		}
		i++
	}
	for k, s := range starts {
		ns := s.pos + s.length
		ne := len(b)
		if k+1 < len(starts) {
			ne = starts[k+1].pos
		}
		if ns < ne {
			fn(b[ns]&0x1f, b[ns:ne])
		}
	}
}

// initVProc поднимает zero-copy конвейер под размер кадра захвата. ok=false → нет
// GPU-конверта, работаем байтовым путём (submit с CPU-конвертированным NV12).
func (e *nativeEncoder) initVProc(srcW, srcH int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return false
	}
	var hr C.int32_t
	rc := C.katana_enc_init_vproc(e.h, C.int(srcW), C.int(srcH), &hr)
	if rc != 0 {
		log.Printf("capture: zero-copy VP unavailable (hr=0x%08x) — CPU NV12 path", uint32(hr))
		return false
	}
	log.Printf("capture: zero-copy active — GPU BGRA→NV12 (VideoProcessor, no CPU copy)")
	return true
}

// captureTexture копирует BGRA-кадр WGC в свою текстуру (пока tex жива, без CPU-копии).
func (e *nativeEncoder) captureTexture(bgraTex uintptr) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("native encoder closed")
	}
	rc := C.katana_enc_capture_texture(e.h, unsafe.Pointer(bgraTex))
	if rc < 0 {
		return fmt.Errorf("native encoder capture_texture: %d", int(rc))
	}
	return nil
}

// encodeCaptured конвертит последний захваченный кадр на GPU и шлёт энкодеру.
func (e *nativeEncoder) encodeCaptured() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("native encoder closed")
	}
	rc := C.katana_enc_encode_captured(e.h)
	if rc < 0 {
		return fmt.Errorf("native encoder encode_captured: %d", int(rc))
	}
	return nil
}

// submit кладёт NV12-кадр в энкодер (неблокирующе).
func (e *nativeEncoder) submit(nv12 []byte) error {
	if len(nv12) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h == nil {
		return errors.New("native encoder closed")
	}
	rc := C.katana_enc_submit(e.h, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.int(len(nv12)))
	if rc < 0 {
		return fmt.Errorf("native encoder submit: %d", int(rc))
	}
	return nil
}

// forceKeyframe просит энкодер выдать IDR на следующем кадре (ответ на PLI зрителя).
func (e *nativeEncoder) forceKeyframe() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h != nil {
		C.katana_enc_force_keyframe(e.h)
	}
}

// setBitrate меняет целевой битрейт на лету (проброс AIMD-регулятора из signaling).
func (e *nativeEncoder) setBitrate(kbps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.h != nil {
		C.katana_enc_set_bitrate(e.h, C.int(kbps))
	}
}

// Close останавливает и освобождает энкодер.
func (e *nativeEncoder) Close() {
	select {
	case <-e.done:
	default:
		close(e.done)
	}
	e.mu.Lock()
	if e.h != nil {
		C.katana_enc_destroy(e.h)
		e.h = nil
	}
	e.mu.Unlock()
}
