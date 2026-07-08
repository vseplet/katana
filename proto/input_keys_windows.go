//go:build windows

// Маппинг клавиш зрителя → Windows Virtual-Key коды (winuser.h VK_*) для SendInput.
// Спец-имена (enter/tab/…) как их шлёт viewer.js, одиночные символы, модификаторы,
// а также таблица HID Usage ID → VK для state-based ввода (keyDownHID/keyUpHID) —
// аналог hidToEvdev на Linux. typeText символы шлёт через Unicode, поэтому таблицы
// рун здесь не нужны.
package main

import "strings"

// Virtual-Key коды (winuser.h). Только используемые.
const (
	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0D
	vkShift  = 0x10
	vkCtrl   = 0x11
	vkMenu   = 0x12 // Alt
	vkPause  = 0x13
	vkCap    = 0x14 // CapsLock
	vkEscape = 0x1B
	vkSpace  = 0x20
	vkPrior  = 0x21 // PageUp
	vkNext   = 0x22 // PageDown
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkInsert = 0x2D
	vkDelete = 0x2E

	vkLWin = 0x5B
	vkRWin = 0x5C
	vkApps = 0x5D

	vkNumpad0  = 0x60
	vkMultiply = 0x6A
	vkAdd      = 0x6B
	vkSubtract = 0x6D
	vkDecimal  = 0x6E
	vkDivide   = 0x6F
	vkF1       = 0x70
	vkF13      = 0x7C

	vkNumLock = 0x90
	vkScroll  = 0x91

	vkLShift = 0xA0
	vkRShift = 0xA1
	vkLCtrl  = 0xA2
	vkRCtrl  = 0xA3
	vkLMenu  = 0xA4
	vkRMenu  = 0xA5

	vkSnapshot = 0x2C // PrintScreen

	vkOEM1     = 0xBA // ;:
	vkOEMPlus  = 0xBB // =+
	vkOEMComma = 0xBC // ,<
	vkOEMMinus = 0xBD // -_
	vkOEMPeriod = 0xBE // .>
	vkOEM2     = 0xBF // /?
	vkOEM3     = 0xC0 // `~
	vkOEM4     = 0xDB // [{
	vkOEM5     = 0xDC // \|
	vkOEM6     = 0xDD // ]}
	vkOEM7     = 0xDE // '"
	vkOEM102   = 0xE2 // <> / ISO backslash
)

// extVK — VK, требующие KEYEVENTF_EXTENDEDKEY (серые навигационные клавиши, правые
// модификаторы, Win/Apps, деление нампада, NumLock). Без этого флага, например,
// стрелки и правый Alt могут отработать не так.
var extVK = map[uint16]bool{
	vkPrior: true, vkNext: true, vkEnd: true, vkHome: true,
	vkLeft: true, vkUp: true, vkRight: true, vkDown: true,
	vkInsert: true, vkDelete: true,
	vkRCtrl: true, vkRMenu: true,
	vkLWin: true, vkRWin: true, vkApps: true,
	vkDivide: true, vkNumLock: true, vkSnapshot: true,
}

// специальные имена клавиш (как в viewer.js KEYMAP).
var specialVK = map[string]uint16{
	"enter": vkReturn, "tab": vkTab, "escape": vkEscape, "esc": vkEscape,
	"backspace": vkBack, "delete": vkDelete, "insert": vkInsert, "space": vkSpace,
	"up": vkUp, "down": vkDown, "left": vkLeft, "right": vkRight,
	"home": vkHome, "end": vkEnd, "pageup": vkPrior, "pagedown": vkNext,
	"f1": vkF1, "f2": vkF1 + 1, "f3": vkF1 + 2, "f4": vkF1 + 3,
	"f5": vkF1 + 4, "f6": vkF1 + 5, "f7": vkF1 + 6, "f8": vkF1 + 7,
	"f9": vkF1 + 8, "f10": vkF1 + 9, "f11": vkF1 + 10, "f12": vkF1 + 11,
}

// одиночные символы (US-раскладка) → VK. Для tapKey/keyDown/keyUp — shortcut'ы
// (ctrl+c и т.п.). Буквы/цифры совпадают с ASCII-кодом заглавной; символы — OEM.
var baseVK = map[rune]uint16{
	'-': vkOEMMinus, '=': vkOEMPlus, '[': vkOEM4, ']': vkOEM6, ';': vkOEM1,
	'\'': vkOEM7, '`': vkOEM3, '\\': vkOEM5, ',': vkOEMComma, '.': vkOEMPeriod,
	'/': vkOEM2, ' ': vkSpace,
}

func modKeyVK(m string) (uint16, bool) {
	switch m {
	case "ctrl", "control":
		return vkCtrl, true
	case "alt", "option":
		return vkMenu, true
	case "shift":
		return vkShift, true
	case "cmd", "meta", "super", "win":
		return vkLWin, true
	}
	return 0, false
}

// keyVK переводит имя клавиши от зрителя в VK (спец-имя или одиночный символ).
func keyVK(key string) (uint16, bool) {
	if c, ok := specialVK[strings.ToLower(key)]; ok {
		return c, true
	}
	rs := []rune(key)
	if len(rs) == 1 {
		r := rs[0]
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A' // буква → VK заглавной (VK_A..VK_Z == 'A'..'Z')
		}
		if r >= 'A' && r <= 'Z' {
			return uint16(r), true
		}
		if r >= '0' && r <= '9' {
			return uint16(r), true // VK_0..VK_9 == '0'..'9'
		}
		if c, ok := baseVK[r]; ok {
			return c, true
		}
	}
	return 0, false
}

// hidToVK — USB HID Usage ID (страница Keyboard, 0x07) → Windows VK. Покрывает все
// клавиши стандартной клавиатуры PC (US ANSI + ISO), аналог hidToEvdev на Linux.
var hidToVK = map[uint8]uint16{
	0x04: 'A', 0x05: 'B', 0x06: 'C', 0x07: 'D', 0x08: 'E', 0x09: 'F',
	0x0A: 'G', 0x0B: 'H', 0x0C: 'I', 0x0D: 'J', 0x0E: 'K', 0x0F: 'L',
	0x10: 'M', 0x11: 'N', 0x12: 'O', 0x13: 'P', 0x14: 'Q', 0x15: 'R',
	0x16: 'S', 0x17: 'T', 0x18: 'U', 0x19: 'V', 0x1A: 'W', 0x1B: 'X',
	0x1C: 'Y', 0x1D: 'Z',
	0x1E: '1', 0x1F: '2', 0x20: '3', 0x21: '4', 0x22: '5',
	0x23: '6', 0x24: '7', 0x25: '8', 0x26: '9', 0x27: '0',
	0x28: vkReturn, // Enter
	0x29: vkEscape,
	0x2A: vkBack, // Backspace
	0x2B: vkTab,
	0x2C: vkSpace,
	0x2D: vkOEMMinus,  // -
	0x2E: vkOEMPlus,   // =
	0x2F: vkOEM4,      // [
	0x30: vkOEM6,      // ]
	0x31: vkOEM5,      // \
	0x33: vkOEM1,      // ;
	0x34: vkOEM7,      // '
	0x35: vkOEM3,      // `
	0x36: vkOEMComma,  // ,
	0x37: vkOEMPeriod, // .
	0x38: vkOEM2,      // /
	0x39: vkCap,       // CapsLock
	0x3A: vkF1, 0x3B: vkF1 + 1, 0x3C: vkF1 + 2, 0x3D: vkF1 + 3, 0x3E: vkF1 + 4, 0x3F: vkF1 + 5, // F1-F6
	0x40: vkF1 + 6, 0x41: vkF1 + 7, 0x42: vkF1 + 8, 0x43: vkF1 + 9, 0x44: vkF1 + 10, 0x45: vkF1 + 11, // F7-F12
	0x46: vkSnapshot, // PrintScreen
	0x47: vkScroll,   // ScrollLock
	0x48: vkPause,    // Pause
	0x49: vkInsert,
	0x4A: vkHome,
	0x4B: vkPrior, // PageUp
	0x4C: vkDelete,
	0x4D: vkEnd,
	0x4E: vkNext, // PageDown
	0x4F: vkRight,
	0x50: vkLeft,
	0x51: vkDown,
	0x52: vkUp,
	0x53: vkNumLock,
	0x54: vkDivide,   // Numpad /
	0x55: vkMultiply, // Numpad *
	0x56: vkSubtract, // Numpad -
	0x57: vkAdd,      // Numpad +
	0x58: vkReturn,   // Numpad Enter
	0x59: vkNumpad0 + 1, 0x5A: vkNumpad0 + 2, 0x5B: vkNumpad0 + 3, // Numpad 1-3
	0x5C: vkNumpad0 + 4, 0x5D: vkNumpad0 + 5, 0x5E: vkNumpad0 + 6, // Numpad 4-6
	0x5F: vkNumpad0 + 7, 0x60: vkNumpad0 + 8, 0x61: vkNumpad0 + 9, // Numpad 7-9
	0x62: vkNumpad0,  // Numpad 0
	0x63: vkDecimal,  // Numpad .
	0x64: vkOEM102,   // IntlBackslash (ISO)
	0x65: vkApps,     // ContextMenu
	0x68: vkF13, 0x69: vkF13 + 1, 0x6A: vkF13 + 2, 0x6B: vkF13 + 3, // F13-F16
	0x6C: vkF13 + 4, 0x6D: vkF13 + 5, 0x6E: vkF13 + 6, 0x6F: vkF13 + 7, // F17-F20
	0x70: vkF13 + 8, 0x71: vkF13 + 9, 0x72: vkF13 + 10, 0x73: vkF13 + 11, // F21-F24
	0xE0: vkLCtrl,
	0xE1: vkLShift,
	0xE2: vkLMenu, // LeftAlt
	0xE3: vkLWin,
	0xE4: vkRCtrl,
	0xE5: vkRShift,
	0xE6: vkRMenu, // RightAlt
	0xE7: vkRWin,
}
