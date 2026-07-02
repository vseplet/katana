//go:build !darwin

// Заглушки инъекции ввода для сборок без нативного управления (headless-linux и
// т.п.): мыши/клавиатуры нет — все операции no-op. Реальный ввод на Linux с
// дисплеем — отдельный display-билд (cgo+X11), добавится позже отдельным файлом.
package main

func moveMouse(x, y int)                    {}
func mouseLocation() (int, int)             { return 0, 0 }
func mouseToggle(button string, down bool)  {}
func dragMouse(x, y int, button string)     {}
func moveRel(dx, dy int)                    {}
func clickMouse(button string)              {}
func doubleClick(button string)             {}
func dragRel(dx, dy int, button string)     {}
func scrollMouse(dx, dy int)                {}
func tapKey(key string, mods []string)      {}
func typeText(s string)                     {}
