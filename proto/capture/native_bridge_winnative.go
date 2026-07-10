//go:build windows && cgo && winnative

package capture

import "log"

// newNativePreferredEncoder — сборка winnative: поднимаем in-process аппаратный
// энкодер (Media Foundation, общий D3D11-девайс с захватом). При неудаче отдаём
// ok=false — тогда firstFrameSetup падает на ffmpeg/MFT-путь.
func newNativePreferredEncoder(dev uintptr, w, h, fps, kbps, gop int) (winVideoEncoder, bool) {
	e, err := newNativeEncoder(dev, w, h, fps, kbps, gop)
	if err != nil {
		log.Printf("capture: native MF encoder unavailable (%v) — fallback", err)
		return nil, false
	}
	return e, true
}
