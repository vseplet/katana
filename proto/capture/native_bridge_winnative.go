//go:build windows && cgo && winnative

package capture

import (
	"context"
	"log"
)

// preferNativeOnly — в нативной сборке ffmpeg/MFT-фолбэка нет: либо поднимается
// нативный энкодер, либо видео нет с чётким HRESULT в логе (чтобы чинить натив, а
// не маскировать провал неработающим ffmpeg).
const preferNativeOnly = true

// newNativePreferredEncoder — сборка winnative: поднимаем in-process аппаратный энкодер
// на общем с захватом D3D11-девайсе. Гейт: есть рантайм AMF (amfrt64.dll, драйвер AMD) →
// AMF-энкодер (даёт слайсы + intra-refresh для локализации потерь, чего MF не даёт),
// иначе — MF-энкодер (Intel/Nvidia/старый драйвер). При неудаче обоих — ok=false.
func newNativePreferredEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int, dropLate bool) (winVideoEncoder, bool) {
	if amfAvailable() {
		if e, err := newAMFEncoder(ctx, frames, dev, w, h, fps, kbps, gop); err == nil {
			return e, true
		} else {
			log.Printf("capture: AMF encoder unavailable (%v) — падаем на MF", err)
		}
	}
	e, err := newNativeEncoder(ctx, frames, dev, w, h, fps, kbps, gop, dropLate)
	if err != nil {
		log.Printf("capture: native MF encoder unavailable (%v) — fallback", err)
		return nil, false
	}
	return e, true
}
