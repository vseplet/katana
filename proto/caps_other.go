//go:build !darwin && !linux && !windows

package main

// hostCaps на прочих платформах (BSD и т.п.): захвата экрана, ввода и звука нет —
// доступен только терминал. Linux считает caps в рантайме отдельно (caps_linux.go),
// macOS — по TCC-правам (caps_darwin.go), Windows — caps_windows.go.
func hostCaps() capsInfo {
	return capsInfo{Video: false, Audio: false, Input: false, Terminal: true}
}
