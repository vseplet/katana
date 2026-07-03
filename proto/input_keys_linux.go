//go:build linux

// Маппинг клавиш зрителя → Linux keycodes (input-event-codes.h) для uinput.
// Спец-имена (enter/tab/…) как их шлёт viewer.js, одиночные символы, модификаторы,
// и таблица рун для typeText (US-раскладка; иные символы v1 пропускает).
package main

import "strings"

const keyLeftShift = 42 // KEY_LEFTSHIFT

// allKeycodes включает весь диапазон обычных клавиш на uinput-клавиатуре, чтобы не
// перечислять каждый код при создании устройства.
func allKeycodes() []int {
	ks := make([]int, 0, 127)
	for c := 1; c <= 127; c++ {
		ks = append(ks, c)
	}
	return ks
}

// специальные имена клавиш (как в viewer.js KEYMAP).
var specialKeys = map[string]uint16{
	"enter": 28, "tab": 15, "escape": 1, "esc": 1, "backspace": 14,
	"delete": 111, "insert": 110, "space": 57,
	"up": 103, "down": 108, "left": 105, "right": 106,
	"home": 102, "end": 107, "pageup": 104, "pagedown": 109,
	"f1": 59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64,
	"f7": 65, "f8": 66, "f9": 67, "f10": 68, "f11": 87, "f12": 88,
}

// буквы/цифры/символы без модификаторов (базовый код клавиши).
var baseKeys = map[rune]uint16{
	'a': 30, 'b': 48, 'c': 46, 'd': 32, 'e': 18, 'f': 33, 'g': 34, 'h': 35,
	'i': 23, 'j': 36, 'k': 37, 'l': 38, 'm': 50, 'n': 49, 'o': 24, 'p': 25,
	'q': 16, 'r': 19, 's': 31, 't': 20, 'u': 22, 'v': 47, 'w': 17, 'x': 45,
	'y': 21, 'z': 44,
	'1': 2, '2': 3, '3': 4, '4': 5, '5': 6, '6': 7, '7': 8, '8': 9, '9': 10, '0': 11,
	'-': 12, '=': 13, '[': 26, ']': 27, ';': 39, '\'': 40, '`': 41, '\\': 43,
	',': 51, '.': 52, '/': 53, ' ': 57,
}

// shifted-символы: рун → базовая клавиша (жать с Shift).
var shiftedKeys = map[rune]uint16{
	'!': 2, '@': 3, '#': 4, '$': 5, '%': 6, '^': 7, '&': 8, '*': 9, '(': 10, ')': 11,
	'_': 12, '+': 13, '{': 26, '}': 27, ':': 39, '"': 40, '~': 41, '|': 43,
	'<': 51, '>': 52, '?': 53,
}

func modKeyCode(m string) (uint16, bool) {
	switch m {
	case "ctrl", "control":
		return 29, true // KEY_LEFTCTRL
	case "alt", "option":
		return 56, true // KEY_LEFTALT
	case "shift":
		return 42, true // KEY_LEFTSHIFT
	case "cmd", "meta", "super", "win":
		return 125, true // KEY_LEFTMETA
	}
	return 0, false
}

// keyCode переводит имя клавиши от зрителя в keycode (спец-имя или одиночный символ).
func keyCode(key string) (uint16, bool) {
	if c, ok := specialKeys[strings.ToLower(key)]; ok {
		return c, true
	}
	rs := []rune(key)
	if len(rs) == 1 {
		r := rs[0]
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A' // KeyTap регистронезависим: буква → базовая клавиша
		}
		if c, ok := baseKeys[r]; ok {
			return c, true
		}
		if c, ok := shiftedKeys[r]; ok {
			return c, true
		}
	}
	return 0, false
}

// runeCode переводит символ для typeText в (keycode, нужен ли Shift).
func runeCode(r rune) (uint16, bool, bool) {
	if r >= 'A' && r <= 'Z' {
		return baseKeys[r+('a'-'A')], true, true
	}
	if c, ok := baseKeys[r]; ok {
		return c, false, true
	}
	if c, ok := shiftedKeys[r]; ok {
		return c, true, true
	}
	return 0, false, false
}
