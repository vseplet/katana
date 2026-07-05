//go:build linux

package main

import "github.com/vseplet/katana/proto/capture"

// hostCaps на Linux: считаем в рантайме — есть графика ($DISPLAY) и ffmpeg →
// видео; есть PulseAudio и ffmpeg → звук; есть доступ к /dev/uinput → ввод
// (мышь/клава через uinput, работает и в X11, и в Wayland). На headless-сервере
// всё false, доступен только терминал.
func hostCaps() capsInfo {
	return capsInfo{
		Video:    capture.VideoAvailable(),
		Audio:    capture.AudioAvailable(),
		Input:        InputAvailable(),
		Terminal:     true,
		Gamepad:      InputAvailable(), // геймпад требует /dev/uinput (тот же гейт, что и ввод)
		MouseCapture: InputAvailable(), // relative-указатель — тот же /dev/uinput
	}
}
