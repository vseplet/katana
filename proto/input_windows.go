//go:build windows

// Инъекция ввода на Windows через Win32 user32 (SendInput / SetCursorPos), без
// cgo — все вызовы идут через LazyDLL. Аналог input_darwin.go (robotgo/CGEvent)
// и input_linux.go (uinput):
//   - мышь: SetCursorPos для абсолютного позиционирования, SendInput с
//     MOUSEEVENTF_* для кнопок/колеса и сырого относительного движения (игры с
//     захватом указателя);
//   - клавиатура: SendInput(KEYBDINPUT) по VK-кодам; typeText — через
//     KEYEVENTF_UNICODE (шлём Unicode-символ напрямую, раскладка не важна);
//   - keyDownHID/keyUpHID — своя таблица HID Usage ID → Windows VK (по аналогии с
//     hidToEvdev на Linux).
//   - геймпад — no-op (виртуальный геймпад под Windows потребовал бы ViGEm/драйвер).
package main

import (
	"runtime"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// --- Win32 биндинги (user32.dll) ------------------------------------------------

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procSetCursorPos = user32.NewProc("SetCursorPos")
	procGetCursorPos = user32.NewProc("GetCursorPos")
	procGetSysMetric = user32.NewProc("GetSystemMetrics")
)

// Константы SendInput (winuser.h).
const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfWheel      = 0x0800
	mouseeventfHWheel     = 0x1000

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002
	keyeventfUnicode  = 0x0004

	wheelDelta = 120 // WHEEL_DELTA — один «нотч» колеса

	smCXScreen = 0
	smCYScreen = 1
)

// mouseInput / keybdInput — содержимое union'а INPUT (winuser.h). Раскладка полей
// точная для 64-бит Windows (LLP64: ULONG_PTR = 8 байт и на amd64, и на arm64).
type mouseInput struct {
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// rawInput — структура INPUT целиком (40 байт на 64-бит): DWORD type + 4 байта
// выравнивания + union (32 байта, размер MOUSEINPUT — он самый большой). Union
// заполняем через unsafe-каст на mouseInput/keybdInput. Поле u — [4]uint64, чтобы
// гарантировать 8-байтное выравнивание (иначе dwExtraInfo лёг бы невыровненно).
type rawInput struct {
	typ uint32
	_   uint32
	u   [4]uint64
}

func (in *rawInput) mouse() *mouseInput { return (*mouseInput)(unsafe.Pointer(&in.u[0])) }
func (in *rawInput) keybd() *keybdInput { return (*keybdInput)(unsafe.Pointer(&in.u[0])) }

// sendInputs шлёт пачку INPUT-событий одним вызовом SendInput.
func sendInputs(ins []rawInput) {
	if len(ins) == 0 {
		return
	}
	procSendInput.Call(
		uintptr(len(ins)),
		uintptr(unsafe.Pointer(&ins[0])),
		unsafe.Sizeof(ins[0]),
	)
}

func mkMouse(flags uint32, dx, dy int32, data uint32) rawInput {
	var in rawInput
	in.typ = inputMouse
	m := in.mouse()
	m.dx, m.dy, m.mouseData, m.dwFlags = dx, dy, data, flags
	return in
}

func mkKey(vk uint16, flags uint32) rawInput {
	var in rawInput
	in.typ = inputKeyboard
	k := in.keybd()
	k.wVk, k.dwFlags = vk, flags
	if extVK[vk] {
		k.dwFlags |= keyeventfExtended
	}
	return in
}

func mkUnicode(u uint16, up bool) rawInput {
	var in rawInput
	in.typ = inputKeyboard
	k := in.keybd()
	k.wScan = u
	k.dwFlags = keyeventfUnicode
	if up {
		k.dwFlags |= keyeventfKeyUp
	}
	return in
}

// --- Экран (для клампа абсолютного позиционирования) ----------------------------

func screenSize() (int, int) {
	w, _, _ := procGetSysMetric.Call(smCXScreen)
	h, _, _ := procGetSysMetric.Call(smCYScreen)
	return int(w), int(h)
}

func clampScr(v, hi int) int {
	if hi <= 0 {
		return v
	}
	if v < 0 {
		return 0
	}
	if v >= hi {
		return hi - 1
	}
	return v
}

// --- Мышь -----------------------------------------------------------------------

// moveMouse — абсолютное позиционирование курсора в пиксели экрана (top-left).
func moveMouse(x, y int) {
	w, h := screenSize()
	procSetCursorPos.Call(uintptr(int32(clampScr(x, w))), uintptr(int32(clampScr(y, h))))
}

// mouseLocation — текущая позиция курсора (пиксели, глобальные). Для подсветки
// курсора у вьюера и follow-pan при зуме.
func mouseLocation() (int, int) {
	pt := new(struct{ x, y int32 }) // out-структура на куче (Go двигает стеки)
	procGetCursorPos.Call(uintptr(unsafe.Pointer(pt)))
	runtime.KeepAlive(pt)
	return int(pt.x), int(pt.y)
}

func mouseDownUpFlags(button string) (down, up uint32) {
	switch button {
	case "right":
		return mouseeventfRightDown, mouseeventfRightUp
	case "center", "middle":
		return mouseeventfMiddleDown, mouseeventfMiddleUp
	default:
		return mouseeventfLeftDown, mouseeventfLeftUp
	}
}

// mouseToggle жмёт/отпускает кнопку мыши по ТЕКУЩЕЙ позиции курсора.
func mouseToggle(button string, down bool) {
	d, u := mouseDownUpFlags(button)
	f := d
	if !down {
		f = u
	}
	sendInputs([]rawInput{mkMouse(f, 0, 0, 0)})
}

// dragMouse — перетаскивание (кнопка уже зажата): достаточно переместить курсор.
func dragMouse(x, y int, button string) { moveMouse(x, y) }

// moveRel — относительный сдвиг курсора (трекпад-режим мобилы): читаем позицию и
// переставляем абсолютно, чтобы модель совпадала с реальным курсором.
func moveRel(dx, dy int) {
	x, y := mouseLocation()
	moveMouse(x+dx, y+dy)
}

// moveRelRaw — сырые относительные дельты (игры с захватом указателя / Pointer
// Lock): MOUSEEVENTF_MOVE без ABSOLUTE даёт относительное движение, которое игра
// читает как обзор мышью. Позицию не ведём — чувствительность применяет игра.
func moveRelRaw(dx, dy int) {
	if dx == 0 && dy == 0 {
		return
	}
	sendInputs([]rawInput{mkMouse(mouseeventfMove, int32(dx), int32(dy), 0)})
}

// releaseAllButtons — отпускает все кнопки мыши. Страховка при выходе из захвата:
// зажатая в игре кнопка не должна «залипнуть».
func releaseAllButtons() {
	sendInputs([]rawInput{
		mkMouse(mouseeventfLeftUp, 0, 0, 0),
		mkMouse(mouseeventfRightUp, 0, 0, 0),
		mkMouse(mouseeventfMiddleUp, 0, 0, 0),
	})
}

func clickMouse(button string) {
	mouseToggle(button, true)
	mouseToggle(button, false)
}

func doubleClick(button string) {
	clickMouse(button)
	clickMouse(button)
}

func dragRel(dx, dy int, button string) { moveRel(dx, dy) } // кнопка уже зажата

// scrollMouse — колесо. Зритель шлёт пиксели; Windows-колесо считает в WHEEL_DELTA
// (120 = один нотч). Знак: у зрителя dy>0 = вниз, у WHEEL положительное = вверх →
// инвертируем вертикаль; горизонталь: вправо = положительное.
func scrollMouse(dx, dy int) {
	var ins []rawInput
	if wy := notches(dy); wy != 0 {
		ins = append(ins, mkMouse(mouseeventfWheel, 0, 0, uint32(int32(-wy*wheelDelta))))
	}
	if wx := notches(dx); wx != 0 {
		ins = append(ins, mkMouse(mouseeventfHWheel, 0, 0, uint32(int32(wx*wheelDelta))))
	}
	sendInputs(ins)
}

func notches(px int) int {
	n := px / 40
	if n == 0 && px != 0 {
		if px > 0 {
			return 1
		}
		return -1
	}
	return n
}

// --- Клавиатура -----------------------------------------------------------------

func tapKey(key string, mods []string) {
	vk, ok := keyVK(key)
	if !ok {
		return
	}
	var modVK []uint16
	for _, m := range mods {
		if c, ok := modKeyVK(m); ok {
			modVK = append(modVK, c)
		}
	}
	ins := make([]rawInput, 0, len(modVK)*2+2)
	for _, c := range modVK {
		ins = append(ins, mkKey(c, 0))
	}
	ins = append(ins, mkKey(vk, 0), mkKey(vk, keyeventfKeyUp))
	for i := len(modVK) - 1; i >= 0; i-- {
		ins = append(ins, mkKey(modVK[i], keyeventfKeyUp))
	}
	sendInputs(ins)
}

func keyDown(key string, mods []string) {
	vk, ok := keyVK(key)
	if !ok {
		return
	}
	var ins []rawInput
	for _, m := range mods {
		if c, ok := modKeyVK(m); ok {
			ins = append(ins, mkKey(c, 0))
		}
	}
	ins = append(ins, mkKey(vk, 0))
	sendInputs(ins)
}

func keyUp(key string, mods []string) {
	vk, ok := keyVK(key)
	if !ok {
		return
	}
	ins := []rawInput{mkKey(vk, keyeventfKeyUp)}
	for i := len(mods) - 1; i >= 0; i-- {
		if c, ok := modKeyVK(mods[i]); ok {
			ins = append(ins, mkKey(c, keyeventfKeyUp))
		}
	}
	sendInputs(ins)
}

// typeText печатает строку через KEYEVENTF_UNICODE — шлём Unicode-код каждого
// символа (down+up), раскладка клавиатуры не участвует. Символы вне BMP кодируем
// суррогатной парой UTF-16.
func typeText(s string) {
	units := utf16.Encode([]rune(s))
	ins := make([]rawInput, 0, len(units)*2)
	for _, u := range units {
		ins = append(ins, mkUnicode(u, false), mkUnicode(u, true))
	}
	sendInputs(ins)
}

// --- Геймпад (не поддержан на Windows) ------------------------------------------
// Виртуальный геймпад под Windows требует драйвера (ViGEmBus) — вне рамок порта.
func gamepadButton(_ int, _ bool, _ float64) {}
func gamepadAxis(_ int, _ float64)           {}

// --- HID → VK (state-based ввод по HID Usage ID) --------------------------------

func keyDownHID(hid uint8) {
	vk, ok := hidToVK[hid]
	if !ok {
		return
	}
	sendInputs([]rawInput{mkKey(vk, 0)})
}

func keyUpHID(hid uint8) {
	vk, ok := hidToVK[hid]
	if !ok {
		return
	}
	sendInputs([]rawInput{mkKey(vk, keyeventfKeyUp)})
}

// InputAvailable — на Windows инъекция ввода доступна всегда (user32 в наличии).
func InputAvailable() bool { return true }
