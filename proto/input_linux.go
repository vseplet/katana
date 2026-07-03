//go:build linux

// Инъекция ввода на Linux через uinput (/dev/uinput) — работает и в X11, и в
// Wayland (эмулирует устройство на уровне ядра, до дисплей-сервера; XTEST на
// Wayland мёртв). Указатель эмулируется как ПЛАНШЕТ С ПЕРОМ (ABS_X/Y + перо в
// proximity + INPUT_PROP_POINTER): libinput/KWin принимают его как абсолютный
// указатель и двигают курсор в пиксель экрана. Клавиатура — отдельное устройство.
//
// Ограничения: клавиатура шлёт скан-коды (символ зависит от раскладки, ок для US/
// ASCII). Клики пера: тип-нажатие = левая, стилус = правая, стилус2 = средняя.
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
	uiSetAbsbit  = 0x40045567
	uiSetPropbit = 0x4004556e // _IOW('U', 110, int)
	uiDevCreate  = 0x5501     // _IO('U', 1)
	uiDevDstry   = 0x5502

	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03

	synReport   = 0x00
	absX        = 0x00
	absY        = 0x01
	absPressure = 0x18
	relWheel    = 0x08
	relHWheel   = 0x06

	btnLeft    = 0x110
	btnRight   = 0x111
	btnMiddle  = 0x112
	btnToolPen = 0x140
	btnTouch   = 0x14a
	btnStylus  = 0x14b
	btnStylus2 = 0x14c

	inputPropPointer = 0x00 // INPUT_PROP_POINTER — курсор следует за ABS (не touchscreen)
	absCnt           = 0x40 // ABS_CNT
)

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

func ioctl(f *os.File, req uintptr, arg uintptr) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, arg); errno != 0 {
		return errno
	}
	return nil
}

// createUinput открывает /dev/uinput, включает события/оси/свойства и создаёт
// устройство. abs — карта ось→максимум (пусто = без абсолютных осей/колеса).
func createUinput(name string, keys []int, abs map[int]int, props []int) (*inputDev, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	must := func(req uintptr, v int) {
		if err == nil {
			err = ioctl(f, req, uintptr(v))
		}
	}
	must(uiSetEvbit, evKey)
	for _, k := range keys {
		must(uiSetKeybit, k)
	}
	for _, p := range props {
		must(uiSetPropbit, p)
	}
	if len(abs) > 0 {
		must(uiSetEvbit, evAbs)
		for code := range abs {
			must(uiSetAbsbit, code)
		}
		must(uiSetEvbit, evRel)
		must(uiSetRelbit, relWheel)
		must(uiSetRelbit, relHWheel)
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	// uinput_user_dev: name[80] + input_id(8) + ff_effects_max(4) +
	// absmax[64]+absmin[64]+absfuzz[64]+absflat[64] (по s32).
	buf := make([]byte, 80+8+4+absCnt*4*4)
	copy(buf[:80], name)
	binary.LittleEndian.PutUint16(buf[80:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint16(buf[82:], 0x1)  // vendor
	binary.LittleEndian.PutUint16(buf[84:], 0x1)  // product
	binary.LittleEndian.PutUint16(buf[86:], 0x1)  // version
	absmax := 80 + 8 + 4                          // начало absmax[]
	for code, max := range abs {
		binary.LittleEndian.PutUint32(buf[absmax+code*4:], uint32(max))
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

// --- Состояние: планшет-указатель + клавиатура + позиция курсора ---
var (
	inMu     sync.Mutex
	ptr      *inputDev
	kbd      *inputDev
	scrW     int
	scrH     int
	curX     int
	curY     int
	inputSet bool
	inputOK  bool
)

// ensureInput лениво поднимает uinput-девайсы при первом событии ввода. Ошибка
// (нет прав на /dev/uinput) логируется, дальше ввод — no-op.
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
	curX, curY = scrW/2, scrH/2

	// Планшет-перо: ABS X/Y (пиксели экрана) + pressure, кнопки пера, свойство
	// «указатель». Так libinput ведёт курсор за абсолютной позицией и в Wayland.
	abs := map[int]int{absX: scrW - 1, absY: scrH - 1, absPressure: 1023}
	keys := []int{btnLeft, btnRight, btnMiddle, btnToolPen, btnTouch, btnStylus, btnStylus2}
	p, err := createUinput("katana-pointer", keys, abs, []int{inputPropPointer})
	if err != nil {
		log.Printf("input: uinput pointer: %v (input disabled; need rw on /dev/uinput)", err)
		return false
	}
	k, err := createUinput("katana-keyboard", allKeycodes(), nil, nil)
	if err != nil {
		log.Printf("input: uinput keyboard: %v (input disabled)", err)
		p.close()
		return false
	}
	ptr, kbd = p, k

	// Перо «в зоне» (proximity) + стартовая позиция → курсор появляется и следует.
	ptr.emit(evKey, btnToolPen, 1)
	ptr.emit(evAbs, absX, int32(curX))
	ptr.emit(evAbs, absY, int32(curY))
	ptr.syn()

	inputOK = true
	log.Printf("input: uinput ready (%dx%d, pen-tablet)", scrW, scrH)
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

// tabletBtn — кнопка зрителя → кнопка пера: левая=тип, правая=стилус, средняя=стилус2.
func tabletBtn(button string) uint16 {
	switch button {
	case "right":
		return btnStylus
	case "center", "middle":
		return btnStylus2
	default:
		return btnTouch
	}
}

// --- Публичный API (сигнатуры совпадают с input_darwin.go / input_other.go) ---

func moveMouse(x, y int) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	curX, curY = clampScr(x, scrW), clampScr(y, scrH)
	ptr.emit(evAbs, absX, int32(curX))
	ptr.emit(evAbs, absY, int32(curY))
	ptr.syn()
}

func mouseLocation() (int, int) {
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
	btn := tabletBtn(button)
	if btn == btnTouch { // левая = касание пером: даём давление, иначе клик может не сработать
		if down {
			ptr.emit(evAbs, absPressure, 1023)
		} else {
			ptr.emit(evAbs, absPressure, 0)
		}
	}
	ptr.emit(evKey, btn, v)
	ptr.syn()
}

func dragMouse(x, y int, button string) { moveMouse(x, y) } // кнопка/касание уже активно

func moveRel(dx, dy int) {
	inMu.Lock()
	x, y := curX+dx, curY+dy
	inMu.Unlock()
	moveMouse(x, y)
}

func clickMouse(button string) {
	mouseToggle(button, true)
	mouseToggle(button, false)
}

func doubleClick(button string) {
	clickMouse(button)
	clickMouse(button)
}

func dragRel(dx, dy int, button string) { moveRel(dx, dy) }

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

func typeText(s string) {
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	for _, r := range s {
		code, shift, ok := runeCode(r)
		if !ok {
			continue // символ вне текущей раскладки — пропускаем (v1)
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
