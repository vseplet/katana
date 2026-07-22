package main

import (
	"github.com/go-vgo/robotgo"

	"github.com/vseplet/katana/proto/capture"
)

// moveMouse перемещает курсор в глобальные координаты (точки, top-left).
func moveMouse(x, y int) { robotgo.Move(x, y) }

// mouseLocation возвращает текущую позицию курсора (глобальные точки) — чтобы
// хост сообщал её вьюеру (подсветка курсора и follow-pan при зуме на мобиле).
func mouseLocation() (int, int) { return robotgo.Location() }

// rbtn — имя кнопки для robotgo: он знает "center", а зритель шлёт "middle" → алиас.
func rbtn(button string) string {
	if button == "middle" {
		return "center"
	}
	return button
}

// mouseToggle жмёт/отпускает кнопку мыши ("left" | "right" | "center"/"middle").
func mouseToggle(button string, down bool) {
	state := "up"
	if down {
		state = "down"
	}
	robotgo.Toggle(rbtn(button), state)
}

// dragMouse постит событие перетаскивания (мышь с зажатой кнопкой) в (x, y).
func dragMouse(x, y int, button string) { robotgo.Drag(x, y, rbtn(button)) }

// moveRel сдвигает курсор на (dx, dy) от текущей позиции — трекпад-режим мобилы
// (свайп = относительное движение, а не абсолютное позиционирование).
func moveRel(dx, dy int) {
	x, y := robotgo.Location()
	robotgo.Move(x+dx, y+dy)
}

// clickMouse кликает кнопкой ("left"|"right"|"middle") по ТЕКУЩЕЙ позиции курсора.
func clickMouse(button string) { robotgo.Click(rbtn(button)) }

// doubleClick — двойной клик по текущей позиции (правильный clickCount=2, чтобы
// ОС распознала double — например, разворот окна по заголовку).
func doubleClick(button string) { robotgo.Click(rbtn(button), true) }

// dragRel тащит на (dx, dy) от текущей позиции с зажатой кнопкой (drag-событие,
// не просто move) — относительное перетаскивание (long-press-drag на мобиле).
func dragRel(dx, dy int, button string) {
	x, y := robotgo.Location()
	robotgo.Drag(x+dx, y+dy, rbtn(button))
}

// scrollMouse прокручивает на dx/dy ПИКСЕЛЕЙ (пиксельно-точно, как трекпад) —
// нативный CGEvent с пиксельными единицами, а не строчный robotgo.Scroll.
func scrollMouse(dx, dy int) { capture.InjectScroll(dx, dy) }

// moveRelRaw — захват мыши для игр (pointer-lock). На macOS отдельного relative-
// устройства нет; отдаём как обычное относительное движение (best-effort).
func moveRelRaw(dx, dy int) { moveRel(dx, dy) }

// releaseAllButtons — страховка при выходе из захвата (на macOS не требуется).
func releaseAllButtons() {}

// tapKey нажимает клавишу key с модификаторами mods ("ctrl"|"alt"|"cmd"|"shift").
// Для спец-клавиш (enter/tab/стрелки/…) и шорткатов (Cmd+C и т.п.).
func tapKey(key string, mods []string) {
	args := make([]interface{}, len(mods))
	for i, m := range mods {
		args[i] = m
	}
	robotgo.KeyTap(key, args...)
}

// typeText печатает произвольный текст (символы/регистр/юникод) — для обычного
// набора (TypeStr корректно обрабатывает shift-символы, в отличие от KeyTap).
func typeText(s string) { robotgo.TypeStr(s) }

// keyDown зажимает клавишу без отпускания (для state-based ввода: зажатые клавиши,
// геймпад WASD, и т.п.). Пара к keyUp.
func keyDown(key string, mods []string) {
	args := make([]interface{}, len(mods))
	for i, m := range mods {
		args[i] = m
	}
	_ = robotgo.KeyDown(key, args...)
}

// keyUp отпускает клавишу, ранее зажатую keyDown.
func keyUp(key string, mods []string) {
	args := make([]interface{}, len(mods))
	for i, m := range mods {
		args[i] = m
	}
	_ = robotgo.KeyUp(key, args...)
}

// gamepadButton и gamepadAxis — заглушки на macOS (нативный HID-геймпад требует
// отдельного драйвера; инъекция через uinput недоступна).
func gamepadButton(_ int, _ bool, _ float64) {}
func gamepadAxis(_ int, _ float64)            {}

// hidToRobotgo — HID Usage ID → robotgo key name.
var hidToRobotgo = map[uint8]string{
	0x04: "a", 0x05: "b", 0x06: "c", 0x07: "d", 0x08: "e", 0x09: "f",
	0x0A: "g", 0x0B: "h", 0x0C: "i", 0x0D: "j", 0x0E: "k", 0x0F: "l",
	0x10: "m", 0x11: "n", 0x12: "o", 0x13: "p", 0x14: "q", 0x15: "r",
	0x16: "s", 0x17: "t", 0x18: "u", 0x19: "v", 0x1A: "w", 0x1B: "x",
	0x1C: "y", 0x1D: "z",
	0x1E: "1", 0x1F: "2", 0x20: "3", 0x21: "4", 0x22: "5",
	0x23: "6", 0x24: "7", 0x25: "8", 0x26: "9", 0x27: "0",
	0x28: "enter", 0x29: "escape", 0x2A: "backspace", 0x2B: "tab", 0x2C: "space",
	0x2D: "-", 0x2E: "=", 0x2F: "[", 0x30: "]", 0x31: "\\",
	0x33: ";", 0x34: "'", 0x35: "`", 0x36: ",", 0x37: ".", 0x38: "/",
	0x39: "capslock",
	0x3A: "f1", 0x3B: "f2", 0x3C: "f3", 0x3D: "f4", 0x3E: "f5", 0x3F: "f6",
	0x40: "f7", 0x41: "f8", 0x42: "f9", 0x43: "f10", 0x44: "f11", 0x45: "f12",
	0x49: "insert", 0x4A: "home", 0x4B: "pageup",
	0x4C: "delete", 0x4D: "end", 0x4E: "pagedown",
	0x4F: "right", 0x50: "left", 0x51: "down", 0x52: "up",
	0xE0: "ctrl", 0xE1: "shift", 0xE2: "alt", 0xE3: "cmd",
	0xE4: "rctrl", 0xE5: "rshift", 0xE6: "ralt", 0xE7: "rcmd",
}

func keyDownHID(hid uint8) {
	if name, ok := hidToRobotgo[hid]; ok {
		_ = robotgo.KeyDown(name)
	}
}

func keyUpHID(hid uint8) {
	if name, ok := hidToRobotgo[hid]; ok {
		_ = robotgo.KeyUp(name)
	}
}
