//go:build windows

package main

import "github.com/vseplet/katana/proto/capture"

// hostCaps на Windows: видео — WGC+Media Foundation (capture.VideoAvailable);
// звук пока не поддержан (нет системного loopback без внешних зависимостей);
// ввод — user32/SendInput (всегда доступен). Геймпад отсутствует (нужен драйвер
// ViGEm). MouseCapture доступен — сырой relative-ввод шлём через SendInput.
func hostCaps() capsInfo {
	return capsInfo{
		Video:        capture.VideoAvailable(),
		Audio:        capture.AudioAvailable(),
		Input:        InputAvailable(),
		Terminal:     true,
		Gamepad:      false,
		MouseCapture: InputAvailable(),
	}
}
