//go:build linux

package main

import "github.com/vseplet/katana/proto/capture"

// hostCaps на Linux: видео/звук считаем в рантайме — есть графика ($DISPLAY) и
// ffmpeg → видео; есть PulseAudio и ffmpeg → звук. На headless-сервере оба false,
// доступен только терминал. Ввод пока не реализован (нет инъекции мыши/клавы).
func hostCaps() capsInfo {
	return capsInfo{
		Video:    capture.VideoAvailable(),
		Audio:    capture.AudioAvailable(),
		Input:    false,
		Terminal: true,
	}
}
