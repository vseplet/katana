//go:build !darwin && !linux

package main

// hostCaps на прочих платформах (Windows/BSD и т.п.): захвата экрана, ввода и
// звука нет — доступен только терминал. Linux считает caps в рантайме отдельно
// (caps_linux.go), macOS — по TCC-правам (caps_darwin.go).
func hostCaps() capsInfo {
	return capsInfo{Video: false, Audio: false, Input: false, Terminal: true}
}
