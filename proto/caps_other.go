//go:build !darwin

package main

// hostCaps на не-macOS (headless-linux и т.п.): захвата экрана, ввода и звука нет
// — доступен только терминал. Display-билд под Linux добавит их позже.
func hostCaps() capsInfo {
	return capsInfo{Video: false, Audio: false, Input: false, Terminal: true}
}
