//go:build linux

// Инъекция ввода на Linux через uinput (/dev/uinput) — работает в X11 и Wayland
// (эмулирует устройство на уровне ядра, до дисплей-сервера). Указатель:
//   - Wayland: АБСОЛЮТНОЕ устройство (ABS_X/ABS_Y, стиль QEMU usb-tablet). libinput/
//     KWin позиционируют курсор 1:1 без ускорения указателя → точность как у портала,
//     но БЕЗ RemoteDesktop-сессии (значит захват можно вести персистируемой
//     ScreenCast-сессией и не показывать диалог каждый запуск).
//   - X11: относительная мышь (дельты; позицию ведём сами, старт — угол 0,0).
// Клавиатура — отдельное uinput-устройство на обеих сессиях.
//
// Конструктор устройств обобщён (EV_KEY/EV_REL/EV_ABS + INPUT_PROP) — под будущий
// виртуальный геймпад (ABS-стики/триггеры + BTN_* кнопки) правок почти не нужно.
package main

import (
	"encoding/binary"
	"log"
	"os"
	"sync"

	"github.com/vseplet/katana/proto/capture"

	"golang.org/x/sys/unix"
)

// --- uinput ioctl / event константы (linux/uinput.h, input-event-codes.h) ---
const (
	uiSetEvbit   = 0x40045564 // _IOW('U', 100, int)
	uiSetKeybit  = 0x40045565
	uiSetRelbit  = 0x40045566
	uiSetAbsbit  = 0x40045567 // _IOW('U', 103, int)
	uiSetPropbit = 0x40045568 // _IOW('U', 104, int)
	uiDevCreate  = 0x5501     // _IO('U', 1)
	uiDevDstry   = 0x5502

	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03

	synReport = 0x00
	relX      = 0x00
	relY      = 0x01
	relHWheel = 0x06
	relWheel  = 0x08

	absX    = 0x00
	absY    = 0x01
	absZ    = 0x02 // левый триггер (геймпад)
	absRX   = 0x03 // правый стик X
	absRY   = 0x04 // правый стик Y
	absRZ   = 0x05 // правый триггер
	absHat0X = 0x10 // крестовина X
	absHat0Y = 0x11 // крестовина Y
	absMax  = 32767 // диапазон абсолютных осей (как QEMU usb-tablet)

	// BTN_* для геймпада Xbox-типа (linux/input-event-codes.h)
	btnGamepadA     = 0x130
	btnGamepadB     = 0x131
	btnGamepadX     = 0x133
	btnGamepadY     = 0x134
	btnGamepadTL    = 0x136 // Left bumper
	btnGamepadTR    = 0x137 // Right bumper
	btnGamepadTL2   = 0x138 // Left trigger (digital)
	btnGamepadTR2   = 0x139 // Right trigger (digital)
	btnGamepadSel   = 0x13a // Select/Back
	btnGamepadStart = 0x13b
	btnGamepadMode  = 0x13c // Guide/Home
	btnGamepadThL   = 0x13d // Left stick click
	btnGamepadThR   = 0x13e // Right stick click

	inputPropPointer = 0x00 // INPUT_PROP_POINTER — «устройству нужен курсор»

	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112

	absCnt = 0x40 // ABS_CNT (для размера uinput_user_dev)
)

// absAxis — одна абсолютная ось uinput-устройства (код + диапазон). Обобщение под
// абсолютный указатель (ABS_X/ABS_Y) и будущий виртуальный геймпад (стики/триггеры).
type absAxis struct{ code, min, max int }

// inputDev — один виртуальный uinput-девайс.
type inputDev struct{ f *os.File }

// emit пишет одно input_event (24 байта на 64-бит: timeval[16] + type + code + value).
func (d *inputDev) emit(typ, code uint16, value int32) {
	var b [24]byte
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(value))
	_, _ = d.f.Write(b[:])
}

func (d *inputDev) syn() { d.emit(evSyn, synReport, 0) }

// absMove — абсолютное позиционирование (ax/ay уже в диапазоне 0..absMax).
func (d *inputDev) absMove(ax, ay int32) {
	d.emit(evAbs, absX, ax)
	d.emit(evAbs, absY, ay)
	d.syn()
}

// scaleAbs — пиксель экрана → абсолютная координата 0..absMax.
func scaleAbs(px, scr int) int32 {
	if scr <= 1 {
		return 0
	}
	v := px * absMax / (scr - 1)
	if v < 0 {
		v = 0
	}
	if v > absMax {
		v = absMax
	}
	return int32(v)
}

func ioctl(f *os.File, req uintptr, arg uintptr) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, arg); errno != 0 {
		return errno
	}
	return nil
}

// createUinput открывает /dev/uinput, включает события/оси/свойства и создаёт
// устройство. keys — EV_KEY коды (кнопки/клавиши), rels — EV_REL оси, abses —
// EV_ABS оси с диапазоном, props — INPUT_PROP_* свойства. Обобщён под указатель,
// клавиатуру и будущий геймпад.
func createUinput(name string, keys, rels []int, abses []absAxis, props []int) (*inputDev, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	must := func(req uintptr, v int) {
		if err == nil {
			err = ioctl(f, req, uintptr(v))
		}
	}
	for _, p := range props {
		must(uiSetPropbit, p)
	}
	if len(keys) > 0 {
		must(uiSetEvbit, evKey)
		for _, k := range keys {
			must(uiSetKeybit, k)
		}
	}
	if len(rels) > 0 {
		must(uiSetEvbit, evRel)
		for _, r := range rels {
			must(uiSetRelbit, r)
		}
	}
	if len(abses) > 0 {
		must(uiSetEvbit, evAbs)
		for _, a := range abses {
			must(uiSetAbsbit, a.code)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	// uinput_user_dev: name[80] + input_id(8) + ff_effects_max(4) +
	// absmax[64]@92 + absmin[64]@348 + absfuzz[64]@604 + absflat[64]@860.
	buf := make([]byte, 80+8+4+absCnt*4*4)
	copy(buf[:80], name)
	binary.LittleEndian.PutUint16(buf[80:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint16(buf[82:], 0x1)  // vendor
	binary.LittleEndian.PutUint16(buf[84:], 0x1)  // product
	binary.LittleEndian.PutUint16(buf[86:], 0x1)  // version
	for _, a := range abses {
		binary.LittleEndian.PutUint32(buf[92+a.code*4:], uint32(int32(a.max)))
		binary.LittleEndian.PutUint32(buf[348+a.code*4:], uint32(int32(a.min)))
	}
	if _, werr := f.Write(buf); werr != nil {
		f.Close()
		return nil, werr
	}
	if err := ioctl(f, uiDevCreate, 0); err != nil {
		f.Close()
		return nil, err
	}
	return &inputDev{f: f}, nil
}

func (d *inputDev) close() {
	if d == nil || d.f == nil {
		return
	}
	_ = ioctl(d.f, uiDevDstry, 0)
	_ = d.f.Close()
}

// --- Состояние: указатель (abs на Wayland / rel на X11) + клавиатура + позиция ---
var (
	inMu     sync.Mutex
	ptr      *inputDev
	kbd      *inputDev
	ptrAbs   bool // указатель абсолютный (Wayland) — иначе относительный (X11)
	scrW     int
	scrH     int
	curX     int
	curY     int
	inputSet bool
	inputOK  bool
)

// onWayland — на Wayland указатель абсолютный (ABS-устройство), на X11 —
// относительный. Клавиатура — uinput на обеих сессиях.
func onWayland() bool { return capture.IsWaylandSession() }

// ensureInput лениво поднимает uinput-девайсы при первом событии ввода: указатель
// (Wayland — абсолютный ABS-указатель без ускорения; X11 — относительная мышь) и
// клавиатуру. Ввод больше НЕ идёт через портал RemoteDesktop — это позволяет вести
// захват отдельной ScreenCast-сессией (которую KDE умеет персистить).
func ensureInput() bool {
	inMu.Lock()
	defer inMu.Unlock()
	if inputSet {
		return inputOK
	}
	inputSet = true

	scrW, scrH = capture.ScreenSize()
	if scrW <= 0 || scrH <= 0 {
		scrW, scrH = 1920, 1080
	}

	if onWayland() {
		// Абсолютный указатель (QEMU usb-tablet-стиль): ABS_X/ABS_Y + кнопки + колесо.
		// libinput/KWin позиционируют курсор 1:1 по абсолюту, БЕЗ ускорения (в отличие
		// от относительной мыши, которая «уплывала») → точность без портала.
		p, err := createUinput("katana-pointer",
			[]int{btnLeft, btnRight, btnMiddle},
			[]int{relWheel, relHWheel},
			[]absAxis{{absX, 0, absMax}, {absY, 0, absMax}},
			[]int{inputPropPointer})
		if err != nil {
			log.Printf("input: uinput abs pointer: %v (input disabled; need rw on /dev/uinput)", err)
			return false
		}
		ptr, ptrAbs = p, true
		curX, curY = scrW/2, scrH/2
	} else {
		p, err := createUinput("katana-pointer",
			[]int{btnLeft, btnRight, btnMiddle},
			[]int{relX, relY, relWheel, relHWheel}, nil, nil)
		if err != nil {
			log.Printf("input: uinput pointer: %v (input disabled; need rw on /dev/uinput)", err)
			return false
		}
		ptr = p
		// «Приземляем» курсор в угол (0,0) — относительное устройство не знает
		// реальной позиции, слэмим в левый-верх, синхронизируя нашу модель.
		ptr.emit(evRel, relX, int32(-scrW*2))
		ptr.emit(evRel, relY, int32(-scrH*2))
		ptr.syn()
		curX, curY = 0, 0
	}

	k, err := createUinput("katana-keyboard", allKeycodes(), nil, nil, nil)
	if err != nil {
		log.Printf("input: uinput keyboard: %v (input disabled)", err)
		if ptr != nil {
			ptr.close()
		}
		return false
	}
	kbd = k

	inputOK = true
	log.Printf("input: uinput ready (%dx%d, ptr abs=%v)", scrW, scrH, ptrAbs)
	return true
}

// InputAvailable — можно ли открыть /dev/uinput на запись (для caps).
func InputAvailable() bool {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func clampScr(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v >= hi {
		return hi - 1
	}
	return v
}

func btnCode(button string) uint16 {
	switch button {
	case "right":
		return btnRight
	case "center", "middle":
		return btnMiddle
	default:
		return btnLeft
	}
}

// --- Публичный API (сигнатуры совпадают с input_darwin.go / input_other.go) ---

func moveMouse(x, y int) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	nx, ny := clampScr(x, scrW), clampScr(y, scrH)
	if ptrAbs {
		ptr.absMove(scaleAbs(nx, scrW), scaleAbs(ny, scrH))
	} else {
		ptr.emit(evRel, relX, int32(nx-curX))
		ptr.emit(evRel, relY, int32(ny-curY))
		ptr.syn()
	}
	curX, curY = nx, ny
}

func mouseLocation() (int, int) {
	// Позицию ведём сами (мы командуем курсором) — для abs это точная последняя
	// заданная точка, для rel — накопленная модель.
	inMu.Lock()
	defer inMu.Unlock()
	return curX, curY
}

func mouseToggle(button string, down bool) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	v := int32(0)
	if down {
		v = 1
	}
	ptr.emit(evKey, btnCode(button), v)
	ptr.syn()
}

func dragMouse(x, y int, button string) { moveMouse(x, y) } // кнопка уже зажата

func moveRel(dx, dy int) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	nx, ny := clampScr(curX+dx, scrW), clampScr(curY+dy, scrH)
	if ptrAbs {
		ptr.absMove(scaleAbs(nx, scrW), scaleAbs(ny, scrH))
	} else {
		ptr.emit(evRel, relX, int32(dx))
		ptr.emit(evRel, relY, int32(dy))
		ptr.syn()
	}
	curX, curY = nx, ny
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

func scrollMouse(dx, dy int) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	// Зритель шлёт пиксели; колесо — в «нотчах» (~40px/нотч). Знак как в GTK:
	// колесо вверх = положительный REL_WHEEL, а dy>0 у зрителя = вниз.
	if wy := notches(dy); wy != 0 {
		ptr.emit(evRel, relWheel, int32(-wy))
	}
	if wx := notches(dx); wx != 0 {
		ptr.emit(evRel, relHWheel, int32(wx))
	}
	ptr.syn()
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

func tapKey(key string, mods []string) {
	if !ensureInput() {
		return
	}
	code, ok := keyCode(key)
	if !ok {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	var modCodes []uint16
	for _, m := range mods {
		if c, ok := modKeyCode(m); ok {
			modCodes = append(modCodes, c)
		}
	}
	for _, c := range modCodes {
		kbd.emit(evKey, c, 1)
	}
	kbd.syn()
	kbd.emit(evKey, code, 1)
	kbd.syn()
	kbd.emit(evKey, code, 0)
	kbd.syn()
	for i := len(modCodes) - 1; i >= 0; i-- {
		kbd.emit(evKey, modCodes[i], 0)
	}
	kbd.syn()
}

func keyDown(key string, mods []string) {
	if !ensureInput() {
		return
	}
	code, ok := keyCode(key)
	if !ok {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	for _, m := range mods {
		if c, ok := modKeyCode(m); ok {
			kbd.emit(evKey, c, 1)
		}
	}
	if len(mods) > 0 {
		kbd.syn()
	}
	kbd.emit(evKey, code, 1)
	kbd.syn()
}

func keyUp(key string, mods []string) {
	if !ensureInput() {
		return
	}
	code, ok := keyCode(key)
	if !ok {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	kbd.emit(evKey, code, 0)
	kbd.syn()
	for i := len(mods) - 1; i >= 0; i-- {
		if c, ok := modKeyCode(mods[i]); ok {
			kbd.emit(evKey, c, 0)
		}
	}
	if len(mods) > 0 {
		kbd.syn()
	}
}

func typeText(s string) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	for _, r := range s {
		code, shift, ok := runeCode(r)
		if !ok {
			continue
		}
		if shift {
			kbd.emit(evKey, keyLeftShift, 1)
			kbd.syn()
		}
		kbd.emit(evKey, code, 1)
		kbd.syn()
		kbd.emit(evKey, code, 0)
		kbd.syn()
		if shift {
			kbd.emit(evKey, keyLeftShift, 0)
			kbd.syn()
		}
	}
}

// --- Геймпад (виртуальный Xbox-подобный геймпад через uinput) ---

// gpBtnCodes — маппинг индекса Gamepad API → BTN_* код uinput.
// Индексы 6,7 (триггеры) и 12-15 (крестовина) обрабатываются отдельно.
var gpBtnCodes = map[int]uint16{
	0: btnGamepadA, 1: btnGamepadB, 2: btnGamepadX, 3: btnGamepadY,
	4: btnGamepadTL, 5: btnGamepadTR,
	8: btnGamepadSel, 9: btnGamepadStart, 10: btnGamepadThL, 11: btnGamepadThR,
	16: btnGamepadMode,
}

var (
	gpMu    sync.Mutex
	gp      *inputDev
	gpReady bool
	hatBtns [4]bool // [0]=up [1]=down [2]=left [3]=right
)

func ensureGamepad() bool {
	gpMu.Lock()
	defer gpMu.Unlock()
	if gpReady {
		return gp != nil
	}
	gpReady = true
	var err error
	gp, err = createUinput("katana-gamepad",
		[]int{
			btnGamepadA, btnGamepadB, btnGamepadX, btnGamepadY,
			btnGamepadTL, btnGamepadTR, btnGamepadTL2, btnGamepadTR2,
			btnGamepadSel, btnGamepadStart, btnGamepadMode,
			btnGamepadThL, btnGamepadThR,
		},
		nil,
		[]absAxis{
			{absX, -32767, 32767},
			{absY, -32767, 32767},
			{absZ, 0, 255},
			{absRX, -32767, 32767},
			{absRY, -32767, 32767},
			{absRZ, 0, 255},
			{absHat0X, -1, 1},
			{absHat0Y, -1, 1},
		},
		nil,
	)
	if err != nil {
		log.Printf("input: uinput gamepad: %v", err)
		gp = nil
	}
	return gp != nil
}

func gamepadButton(btn int, down bool, val float64) {
	if !ensureGamepad() {
		return
	}
	gpMu.Lock()
	defer gpMu.Unlock()
	// Триггеры: аналоговая ось + цифровая кнопка
	if btn == 6 {
		gp.emit(evAbs, absZ, int32(val*255))
		b := int32(0)
		if down {
			b = 1
		}
		gp.emit(evKey, btnGamepadTL2, b)
		gp.syn()
		return
	}
	if btn == 7 {
		gp.emit(evAbs, absRZ, int32(val*255))
		b := int32(0)
		if down {
			b = 1
		}
		gp.emit(evKey, btnGamepadTR2, b)
		gp.syn()
		return
	}
	// Крестовина: кнопки 12-15 → ABS_HAT0X/Y
	if btn >= 12 && btn <= 15 {
		hatBtns[btn-12] = down
		x := int32(0)
		y := int32(0)
		if hatBtns[2] {
			x = -1
		} else if hatBtns[3] {
			x = 1
		}
		if hatBtns[0] {
			y = -1
		} else if hatBtns[1] {
			y = 1
		}
		gp.emit(evAbs, absHat0X, x)
		gp.emit(evAbs, absHat0Y, y)
		gp.syn()
		return
	}
	// Обычные кнопки
	code, ok := gpBtnCodes[btn]
	if !ok {
		return
	}
	v := int32(0)
	if down {
		v = 1
	}
	gp.emit(evKey, code, v)
	gp.syn()
}

func gamepadAxis(axis int, val float64) {
	if !ensureGamepad() {
		return
	}
	gpMu.Lock()
	defer gpMu.Unlock()
	v := int32(val * 32767)
	if v < -32767 {
		v = -32767
	}
	if v > 32767 {
		v = 32767
	}
	var code uint16
	switch axis {
	case 0:
		code = absX
	case 1:
		code = absY
	case 2:
		code = absRX
	case 3:
		code = absRY
	default:
		return
	}
	gp.emit(evAbs, code, v)
	gp.syn()
}
