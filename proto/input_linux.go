//go:build linux

// Инъекция ввода на Linux через uinput (/dev/uinput) — работает в X11 и Wayland
// (эмулирует устройство на уровне ядра, до дисплей-сервера). Указатель — ОТНО-
// СИТЕЛЬНАЯ мышь (KWin/libinput в Wayland двигают курсор от неё, в отличие от
// абсолютных ABS/tablet-устройств, которые композитор игнорирует). Позицию курсора
// ведём сами: при старте «приземляем» его в угол (0,0), дальше двигаем дельтами.
// Клавиатура — отдельное устройство.
//
// Ограничение: из-за ускорения указателя libinput абсолютное позиционирование
// может слегка «уплывать» на резких движениях. Точный путь — портал RemoteDesktop.
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
	uiSetEvbit  = 0x40045564 // _IOW('U', 100, int)
	uiSetKeybit = 0x40045565
	uiSetRelbit = 0x40045566
	uiDevCreate = 0x5501 // _IO('U', 1)
	uiDevDstry  = 0x5502

	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02

	synReport = 0x00
	relX      = 0x00
	relY      = 0x01
	relHWheel = 0x06
	relWheel  = 0x08

	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112

	absCnt = 0x40 // ABS_CNT (для размера uinput_user_dev)
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

// createUinput открывает /dev/uinput, включает события/оси и создаёт устройство.
func createUinput(name string, keys []int, rel []int) (*inputDev, error) {
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
	if len(rel) > 0 {
		must(uiSetEvbit, evRel)
		for _, r := range rel {
			must(uiSetRelbit, r)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}

	// uinput_user_dev: name[80] + input_id(8) + ff_effects_max(4) + abs-массивы.
	buf := make([]byte, 80+8+4+absCnt*4*4)
	copy(buf[:80], name)
	binary.LittleEndian.PutUint16(buf[80:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint16(buf[82:], 0x1)  // vendor
	binary.LittleEndian.PutUint16(buf[84:], 0x1)  // product
	binary.LittleEndian.PutUint16(buf[86:], 0x1)  // version
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

// --- Состояние: относительная мышь + клавиатура + позиция курсора ---
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

// onWayland — ввод мыши идёт через портал RemoteDesktop (абсолютные координаты
// через композитор), а не через uinput. Клавиатура на обеих сессиях — uinput.
func onWayland() bool { return capture.IsWaylandSession() }

// ensureInput лениво поднимает uinput-девайсы при первом событии ввода. На Wayland
// поднимает ТОЛЬКО клавиатуру (мышь — через портал); на X11 — мышь+клавиатуру.
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

	if !onWayland() {
		p, err := createUinput("katana-pointer",
			[]int{btnLeft, btnRight, btnMiddle}, []int{relX, relY, relWheel, relHWheel})
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

	k, err := createUinput("katana-keyboard", allKeycodes(), nil)
	if err != nil {
		log.Printf("input: uinput keyboard: %v (input disabled)", err)
		if ptr != nil {
			ptr.close()
		}
		return false
	}
	kbd = k

	inputOK = true
	if onWayland() {
		log.Printf("input: keyboard=uinput, mouse=portal (Wayland, %dx%d)", scrW, scrH)
	} else {
		log.Printf("input: uinput ready (%dx%d, relative)", scrW, scrH)
	}
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
	if onWayland() {
		capture.PortalPointerMotion(float64(x), float64(y)) // абсолют через композитор
		inMu.Lock()
		curX, curY = x, y
		inMu.Unlock()
		return
	}
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	nx, ny := clampScr(x, scrW), clampScr(y, scrH)
	ptr.emit(evRel, relX, int32(nx-curX))
	ptr.emit(evRel, relY, int32(ny-curY))
	ptr.syn()
	curX, curY = nx, ny
}

func mouseLocation() (int, int) {
	inMu.Lock()
	defer inMu.Unlock()
	return curX, curY
}

func mouseToggle(button string, down bool) {
	if onWayland() {
		capture.PortalPointerButton(int32(btnCode(button)), down)
		return
	}
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
	if onWayland() {
		capture.PortalPointerMotionRel(float64(dx), float64(dy))
		inMu.Lock()
		curX, curY = clampScr(curX+dx, scrW), clampScr(curY+dy, scrH)
		inMu.Unlock()
		return
	}
	if !ensureInput() {
		return
	}
	inMu.Lock()
	defer inMu.Unlock()
	ptr.emit(evRel, relX, int32(dx))
	ptr.emit(evRel, relY, int32(dy))
	ptr.syn()
	curX, curY = clampScr(curX+dx, scrW), clampScr(curY+dy, scrH)
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
	if onWayland() {
		// Портал принимает пиксельную прокрутку (dy>0 = вниз, как у зрителя).
		capture.PortalPointerAxis(float64(dx), float64(dy))
		return
	}
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
