//go:build darwin

package capture

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework CoreFoundation /opt/homebrew/lib/libopus.a
#include <stdlib.h>
#include <opus/opus.h>
struct VTEnc;
int vt_open(int handle, int w, int h, int fps, int bitrateKbps, struct VTEnc **out);
int vt_reopen(struct VTEnc *e, int w, int h, int fps, int bitrateKbps);
int vt_encode(struct VTEnc *e, void *bgra, int w, int h, long long ptsNum, int fps, int forceKey);
void vt_set_bitrate(struct VTEnc *e, int bitrateKbps);
void vt_close(struct VTEnc *e);
int opus_enc_create(int rate, int channels, OpusEncoder **out);
int opus_enc_frame(OpusEncoder *enc, const float *pcm, int frameSize, unsigned char *out, int maxBytes);
void opus_enc_destroy(OpusEncoder *enc);
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"unsafe"
)

// nativeFrames: handle → канал закодированных H264 access unit'ов (для goVTFrame).
// nativeOutN: handle → счётчик выданных энкодером кадров (health-check застоя VT).
var (
	nativeMu     sync.Mutex
	nativeFrames = map[int]chan []byte{}
	nativeOutN   = map[int]int64{}
)

func nativeRegister(handle int, ch chan []byte) {
	nativeMu.Lock()
	nativeFrames[handle] = ch
	nativeOutN[handle] = 0
	nativeMu.Unlock()
}

// nativeClose удаляет канал из реестра и закрывает его ПОД мьютексом — так
// goVTFrame (из колбэка VideoToolbox) не отправит в уже закрытый канал.
func nativeClose(handle int, ch chan []byte) {
	nativeMu.Lock()
	delete(nativeFrames, handle)
	delete(nativeOutN, handle)
	close(ch)
	nativeMu.Unlock()
}

// nativeOutCount — сколько кадров энкодер выдал для handle (для детекта застоя).
func nativeOutCount(handle int) int64 {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeOutN[handle]
}

//export goVTFrame
func goVTFrame(handle C.int, buf unsafe.Pointer, length C.int, keyframe C.int) {
	_ = keyframe
	frame := C.GoBytes(buf, length)
	nativeMu.Lock()
	nativeOutN[int(handle)]++
	if ch := nativeFrames[int(handle)]; ch != nil {
		select {
		case ch <- frame:
		default: // потребитель не успевает — роняем кадр (кейфрейм придёт через ≤1с)
		}
	}
	nativeMu.Unlock()
}

// vtEncoder — аппаратный H264-энкодер (VideoToolbox).
// mu сериализует нативный доступ к сессии (encode/reopen/bitrate/close), т.к.
// vt_reopen пересоздаёт сессию, а setBitrate зовётся из другой горутины.
type vtEncoder struct {
	mu       sync.Mutex
	ptr      *C.struct_VTEnc
	handle   int
	w, h     int
	fps      int
	bitrate  int
	pts      C.longlong
	forceKey int32 // atomic: 1 → следующий кадр кодировать как keyframe (по PLI)
	lastOut  int64 // выхлоп энкодера на прошлом encode (детект молчащей сессии)
	stall    int   // сколько encode'ов подряд без нового выхлопа
}

func newVTEncoder(handle, w, h, fps, bitrateKbps int) (*vtEncoder, error) {
	var p *C.struct_VTEnc
	if rc := C.vt_open(C.int(handle), C.int(w), C.int(h), C.int(fps), C.int(bitrateKbps), &p); rc != 0 {
		return nil, fmt.Errorf("vt_open rc=%d", int(rc))
	}
	return &vtEncoder{ptr: p, handle: handle, w: w, h: h, fps: fps, bitrate: bitrateKbps}, nil
}

// encode кодирует один BGRA-кадр (tight, w*4 на строку). Результат уходит в
// goVTFrame асинхронно. Если взведён forceKey (по PLI) — кадр будет keyframe.
// Если сессия отдала ошибку ИЛИ молча перестала выдавать кадры — пересоздаём её
// (VTCompressionSession иногда сваливается в невалидное состояние после событий
// GPU/дисплея/памяти и тихо замолкает — раньше это морозило трансляцию навсегда).
func (e *vtEncoder) encode(bgra []byte) {
	if e == nil || len(bgra) < e.w*e.h*4 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return
	}
	fk := C.int(0)
	if atomic.SwapInt32(&e.forceKey, 0) != 0 {
		fk = 1
	}
	rc := C.vt_encode(e.ptr, unsafe.Pointer(&bgra[0]), C.int(e.w), C.int(e.h), e.pts, C.int(e.fps), fk)
	e.pts++
	if int(rc) != 0 {
		log.Printf("capture: vt_encode rc=%d → recreating VideoToolbox session", int(rc))
		e.reopenLocked()
		return
	}
	// Health-check: encode идёт, но энкодер молчит ~2с → сессия умерла тихо.
	out := nativeOutCount(e.handle)
	if out != e.lastOut {
		e.lastOut = out
		e.stall = 0
	} else {
		e.stall++
		if e.stall >= e.fps*2 {
			log.Printf("capture: no encoder output ~2s → recreating VideoToolbox session")
			e.reopenLocked()
		}
	}
}

// reopenLocked пересоздаёт VT-сессию на месте. Вызывать под e.mu.
func (e *vtEncoder) reopenLocked() {
	if e.ptr == nil {
		return
	}
	rc := C.vt_reopen(e.ptr, C.int(e.w), C.int(e.h), C.int(e.fps), C.int(e.bitrate))
	e.stall = 0
	e.lastOut = nativeOutCount(e.handle)
	if int(rc) != 0 {
		log.Printf("capture: vt_reopen rc=%d", int(rc))
		return
	}
	atomic.StoreInt32(&e.forceKey, 1) // сразу keyframe после пересоздания
}

// requestKeyframe взводит флаг: следующий encode выдаст keyframe (ответ на PLI).
func (e *vtEncoder) requestKeyframe() {
	if e != nil {
		atomic.StoreInt32(&e.forceKey, 1)
	}
}

// setBitrate меняет целевой битрейт энкодера на лету (kbps; для адаптации к сети).
func (e *vtEncoder) setBitrate(kbps int) {
	if e == nil || kbps <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bitrate = kbps
	if e.ptr != nil {
		C.vt_set_bitrate(e.ptr, C.int(kbps))
	}
}

func (e *vtEncoder) close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr != nil {
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
