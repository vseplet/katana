//go:build windows && !(winnative && cgo)

package capture

import "context"

// preferNativeOnly — обычная сборка: нативного энкодера нет, используем ffmpeg/MFT.
const preferNativeOnly = false

// newNativePreferredEncoder — обычная сборка (без cgo/winnative): нативного
// in-process энкодера нет, всегда ok=false → используется ffmpeg/MFT-путь.
func newNativePreferredEncoder(ctx context.Context, frames chan []byte, dev uintptr, w, h, fps, kbps, gop int, dropLate bool) (winVideoEncoder, bool) {
	return nil, false
}
