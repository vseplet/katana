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
	"fmt"
	"log"
	"unsafe"
)

// nativeEncoder — обёртка над katana_enc (C). НЕ потокобезопасна для параллельных
// submit; вызывается из одного потока захвата (как и ffmpeg-путь).
type nativeEncoder struct {
	h   *C.katana_enc
	out []byte // переиспользуемый буфер под один AU
}

// newNativeEncoder поднимает аппаратный H264-MFT на D3D11-устройстве WGC-захвата.
func newNativeEncoder(dev uintptr, w, h, fps, kbps, gop int) (*nativeEncoder, error) {
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
	// Один AU H264 заведомо меньше несжатого кадра — берём его как потолок буфера.
	return &nativeEncoder{h: handle, out: make([]byte, w*h*3/2)}, nil
}

// submit кладёт NV12-кадр в энкодер (неблокирующе).
func (e *nativeEncoder) submit(nv12 []byte) error {
	if len(nv12) == 0 {
		return nil
	}
	rc := C.katana_enc_submit(e.h, (*C.uint8_t)(unsafe.Pointer(&nv12[0])), C.int(len(nv12)))
	if rc < 0 {
		return fmt.Errorf("native encoder submit: %d", int(rc))
	}
	return nil
}

// drain забирает все готовые Annex-B access unit'ы (может быть 0..N за вызов).
func (e *nativeEncoder) drain() [][]byte {
	var aus [][]byte
	for {
		n := int(C.katana_enc_poll(e.h, (*C.uint8_t)(unsafe.Pointer(&e.out[0])), C.int(len(e.out))))
		if n <= 0 {
			// -2 = не влез в буфер: увеличиваем и пробуем снова на следующем вызове.
			if n == -2 {
				e.out = make([]byte, len(e.out)*2)
			}
			break
		}
		au := make([]byte, n)
		copy(au, e.out[:n])
		aus = append(aus, au)
	}
	return aus
}

// Close останавливает и освобождает энкодер.
func (e *nativeEncoder) Close() {
	if e.h != nil {
		C.katana_enc_destroy(e.h)
		e.h = nil
	}
}
