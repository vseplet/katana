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

// newNativePreferredEncoder — сборка winnative: поднимаем in-process аппаратный
// энкодер (Media Foundation, общий D3D11-девайс с захватом). При неудаче отдаём
// ok=false — тогда firstFrameSetup падает на ffmpeg/MFT-путь.
func newNativePreferredEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int, dropLate bool) (winVideoEncoder, bool) {
	e, err := newNativeEncoder(ctx, frames, dev, w, h, fps, kbps, gop, dropLate)
	if err != nil {
		log.Printf("capture: native MF encoder unavailable (%v) — fallback", err)
		return nil, false
	}
	return e, true
}
