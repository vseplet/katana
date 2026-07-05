//go:build !darwin && !linux

// Заглушки инъекции ввода для платформ без нативного управления (Windows/BSD):
// мыши/клавиатуры нет — все операции no-op. macOS — robotgo/CGEvent
// (input_darwin.go), Linux — uinput (input_linux.go, input_keys_linux.go).
package main

func moveMouse(x, y int)                   {}
func mouseLocation() (int, int)            { return 0, 0 }
func mouseToggle(button string, down bool) {}
func dragMouse(x, y int, button string)    {}
func moveRel(dx, dy int)                   {}
func moveRelRaw(dx, dy int)                {}
func releaseAllButtons()                   {}
func clickMouse(button string)             {}
func doubleClick(button string)            {}
func dragRel(dx, dy int, button string)    {}
func scrollMouse(dx, dy int)               {}
func tapKey(key string, mods []string)              {}
func keyDown(key string, mods []string)             {}
func keyUp(key string, mods []string)               {}
func typeText(s string)                             {}
func gamepadButton(_ int, _ bool, _ float64)        {}
func gamepadAxis(_ int, _ float64)                  {}
