//go:build windows && !(winnative && cgo)

package capture

// preferNativeOnly — обычная сборка: нативного энкодера нет, используем ffmpeg/MFT.
const preferNativeOnly = false

// newNativePreferredEncoder — обычная сборка (без cgo/winnative): нативного
// in-process энкодера нет, всегда ok=false → используется ffmpeg/MFT-путь.
func newNativePreferredEncoder(dev uintptr, w, h, fps, kbps, gop int) (winVideoEncoder, bool) {
	return nil, false
}
