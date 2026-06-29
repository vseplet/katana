//go:build darwin

package capture

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework CoreFoundation /opt/homebrew/lib/libopus.a
#include <stdlib.h>
#include <opus/opus.h>
struct VTEnc;
int vt_open(int handle, int w, int h, int fps, int bitrateKbps, struct VTEnc **out);
int vt_encode(struct VTEnc *e, void *bgra, int w, int h, long long ptsNum, int fps);
void vt_close(struct VTEnc *e);
int opus_enc_create(int rate, int channels, OpusEncoder **out);
int opus_enc_frame(OpusEncoder *enc, const float *pcm, int frameSize, unsigned char *out, int maxBytes);
void opus_enc_destroy(OpusEncoder *enc);
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// nativeFrames: handle → канал закодированных H264 access unit'ов (для goVTFrame).
var (
	nativeMu     sync.Mutex
	nativeFrames = map[int]chan []byte{}
)

func nativeRegister(handle int, ch chan []byte) {
	nativeMu.Lock()
	nativeFrames[handle] = ch
	nativeMu.Unlock()
}

// nativeClose удаляет канал из реестра и закрывает его ПОД мьютексом — так
// goVTFrame (из колбэка VideoToolbox) не отправит в уже закрытый канал.
func nativeClose(handle int, ch chan []byte) {
	nativeMu.Lock()
	delete(nativeFrames, handle)
	close(ch)
	nativeMu.Unlock()
}

//export goVTFrame
func goVTFrame(handle C.int, buf unsafe.Pointer, length C.int, keyframe C.int) {
	_ = keyframe
	frame := C.GoBytes(buf, length)
	nativeMu.Lock()
	if ch := nativeFrames[int(handle)]; ch != nil {
		select {
		case ch <- frame:
		default: // потребитель не успевает — роняем кадр (кейфрейм придёт через ≤1с)
		}
	}
	nativeMu.Unlock()
}

// vtEncoder — аппаратный H264-энкодер (VideoToolbox).
type vtEncoder struct {
	ptr     *C.struct_VTEnc
	w, h    int
	fps     int
	pts     C.longlong
}

func newVTEncoder(handle, w, h, fps, bitrateKbps int) (*vtEncoder, error) {
	var p *C.struct_VTEnc
	if rc := C.vt_open(C.int(handle), C.int(w), C.int(h), C.int(fps), C.int(bitrateKbps), &p); rc != 0 {
		return nil, fmt.Errorf("vt_open rc=%d", int(rc))
	}
	return &vtEncoder{ptr: p, w: w, h: h, fps: fps}, nil
}

// encode кодирует один BGRA-кадр (tight, w*4 на строку). Результат уходит в
// goVTFrame асинхронно.
func (e *vtEncoder) encode(bgra []byte) {
	if e == nil || e.ptr == nil || len(bgra) < e.w*e.h*4 {
		return
	}
	C.vt_encode(e.ptr, unsafe.Pointer(&bgra[0]), C.int(e.w), C.int(e.h), e.pts, C.int(e.fps))
	e.pts++
}

func (e *vtEncoder) close() {
	if e != nil && e.ptr != nil {
		C.vt_close(e.ptr)
		e.ptr = nil
	}
}

// opusFrame — сэмплов на канал в 20 мс при 48 кГц.
const opusFrame = 960

// opusEncoder кодирует interleaved-стерео float PCM в Opus-пакеты.
type opusEncoder struct {
	ptr *C.OpusEncoder
	buf []float32
}

func newOpusEncoder() (*opusEncoder, error) {
	var p *C.OpusEncoder
	if rc := C.opus_enc_create(48000, 2, &p); rc != 0 {
		return nil, fmt.Errorf("opus_enc_create rc=%d", int(rc))
	}
	return &opusEncoder{ptr: p}, nil
}

// feed копит PCM и возвращает готовые 20-мс Opus-пакеты.
func (e *opusEncoder) feed(pcm []float32) [][]byte {
	e.buf = append(e.buf, pcm...)
	need := opusFrame * 2 // стерео interleaved
	var pkts [][]byte
	for len(e.buf) >= need {
		var out [4000]byte
		n := C.opus_enc_frame(e.ptr,
			(*C.float)(unsafe.Pointer(&e.buf[0])), C.int(opusFrame),
			(*C.uchar)(unsafe.Pointer(&out[0])), C.int(len(out)))
		if int(n) > 0 {
			pkt := make([]byte, int(n))
			copy(pkt, out[:int(n)])
			pkts = append(pkts, pkt)
		}
		e.buf = e.buf[need:]
	}
	return pkts
}

func (e *opusEncoder) close() {
	if e != nil && e.ptr != nil {
		C.opus_enc_destroy(e.ptr)
		e.ptr = nil
	}
}
