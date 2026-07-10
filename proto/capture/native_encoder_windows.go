//go:build windows && cgo && winnative

// Нативный in-process аппаратный H264-энкодер (Media Foundation, async hardware MFT),
// делящий D3D11-устройство с WGC-захватом — как OBS/Sunshine. Реализация COM на C в
// mf_encoder_windows.c; здесь — тонкие Go-биндинги. Собирается только под тегом
// winnative (cgo на виндовом раннере); обычный релиз (CGO_ENABLED=0) живёт на ffmpeg.

package capture

/*
#cgo CFLAGS: -D_WIN32_WINNT=0x0A00 -DCOBJMACROS
#cgo LDFLAGS: -lmfplat -lmfuuid -lmf -lmfreadwrite -lstrmiids -lwmcodecdspuuid -ld3d11 -ldxgi -lole32 -loleaut32 -luuid

#include <stdlib.h>
#include "mf_encoder_windows.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
	"unsafe"
)

// nativeEncoder — обёртка над katana_enc (C). Выход MFT (Annex-B AU) льём в io.Pipe
// как непрерывный поток и гоним через тот же readH264KeepHeaders, что и ffmpeg-путь:
// он повторяет SPS/PPS перед каждым IDR (иначе зритель, зашедший в середину потока
// или потерявший пакет, разваливается — стрим «пропадает») и держит backpressure.
type nativeEncoder struct {
	h    *C.katana_enc
	pw   *io.PipeWriter
	mu   sync.Mutex    // сериализует доступ к h между poll-горутиной, submit и Close
	done chan struct{} // закрывается в Close, чтобы остановить poll-горутину
}

// newNativeEncoder поднимает аппаратный H264-MFT на D3D11-устройстве WGC-захвата и
// запускает конвейер вывода: poll-горутина → io.Pipe → readH264KeepHeaders → frames.
func newNativeEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int, dropLate bool) (*nativeEncoder, error) {
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

	pr, pw := io.Pipe()
	e := &nativeEncoder{h: handle, pw: pw, done: make(chan struct{})}
	// Читатель: тот же путь, что у ffmpeg — парсит NAL, повторяет SPS/PPS, кладёт в frames.
	// dropLate=true форсим всегда: realtime-экран, устаревший кадр бесполезен — свежесть
	// важнее полноты, а иначе pushFrame блокирует пайп при заторе сети/зрителя.
	_ = dropLate
	go func() {
		readH264KeepHeaders(ctx, pr, frames, true)
		pr.Close()
	}()
	// Писатель: тянет готовые AU из C и льёт в pipe.
	go e.pollLoop()
	return e, nil
}

// pollLoop опрашивает C-энкодер и пишет готовые Annex-B AU в pipe непрерывным потоком.
func (e *nativeEncoder) pollLoop() {
	buf := make([]byte, 1<<20) // 1 MiB под один AU; растёт при -2
	logged := false
	// Диагностика реального битрейта/частоты вывода — чтобы видеть, держит ли MFT CBR.
	var winBytes, winAUs int
	winStart := time.Now()
	for {
		select {
		case <-e.done:
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
			time.Sleep(2 * time.Millisecond)
			continue
		}
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
		if _, err := e.pw.Write(buf[:n]); err != nil {
			return
		}
	}
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
	e.pw.Close() // разблокирует pollLoop, если он завис в pw.Write
	e.mu.Lock()
	if e.h != nil {
		C.katana_enc_destroy(e.h)
		e.h = nil
	}
	e.mu.Unlock()
}
