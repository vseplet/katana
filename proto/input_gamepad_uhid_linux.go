//go:build linux

// Виртуальный геймпад через /dev/uhid — HID-устройство, а не чистый evdev (uinput).
//
// Зачем uhid, а не uinput: uinput создаёт ТОЛЬКО evdev-ноду. Steam Input, не имея
// hidraw, вынужден забирать её эксклюзивно (EVIOCGRAB) → браузер и не-Steam
// приложения перестают видеть геймпад. Физический Xbox так не страдает: он —
// HID-устройство, Steam читает его через hidraw/HIDAPI, а evdev остаётся свободным
// для всех. uhid даёт нам ровно это: ядро создаёт из нашего HID-устройства
// hidraw + evdev + js одновременно → Steam берёт hidraw, браузер берёт evdev.
//
// Мы копируем 1:1 HID report descriptor реального «Xbox Wireless Controller»
// (Bluetooth, 045e:0b13) — тот же дескриптор, что BlueZ отдаёт для железного пада.
// Тогда ядерный драйвер hid-microsoft биндится и раскладывает оси/кнопки в evdev
// в точности как у настоящего, а Steam HIDAPI-драйвер узнаёт его по VID/PID.
//
// Требует доступа к /dev/uhid. Штатно он root-only; нужен udev-правило с uaccess
// (как у /dev/uinput). Если доступа нет — вызывающий код откатывается на uinput.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// --- uhid протокол (uapi/linux/uhid.h), struct uhid_event __packed ---
const (
	uhidDestroy        = 1
	uhidGetReport      = 9
	uhidGetReportReply = 10
	uhidCreate2        = 11
	uhidInput2         = 12
	uhidSetReport      = 13
	uhidSetReportReply = 14

	busBluetooth = 0x05
)

// gamepadHIDDesc — HID report descriptor «Xbox Wireless Controller» (045e:0b13,
// BT), снятый с живого устройства (/sys/.../report_descriptor). INPUT-репорт
// (Report ID 0x01, 17 байт): LX,LY,RX,RY u16 (0..65535, центр 0x8000); LT,RT
// 10-бит (0..1023); hat 4-бит; 15 кнопок; 1 бит Share (Consumer Record).
var gamepadHIDDesc = []byte{
	0x05, 0x01, 0x09, 0x05, 0xa1, 0x01, 0x85, 0x01, 0x09, 0x01, 0xa1, 0x00, 0x09, 0x30, 0x09, 0x31,
	0x15, 0x00, 0x27, 0xff, 0xff, 0x00, 0x00, 0x95, 0x02, 0x75, 0x10, 0x81, 0x02, 0xc0, 0x09, 0x01,
	0xa1, 0x00, 0x09, 0x32, 0x09, 0x35, 0x15, 0x00, 0x27, 0xff, 0xff, 0x00, 0x00, 0x95, 0x02, 0x75,
	0x10, 0x81, 0x02, 0xc0, 0x05, 0x02, 0x09, 0xc5, 0x15, 0x00, 0x26, 0xff, 0x03, 0x95, 0x01, 0x75,
	0x0a, 0x81, 0x02, 0x15, 0x00, 0x25, 0x00, 0x75, 0x06, 0x95, 0x01, 0x81, 0x03, 0x05, 0x02, 0x09,
	0xc4, 0x15, 0x00, 0x26, 0xff, 0x03, 0x95, 0x01, 0x75, 0x0a, 0x81, 0x02, 0x15, 0x00, 0x25, 0x00,
	0x75, 0x06, 0x95, 0x01, 0x81, 0x03, 0x05, 0x01, 0x09, 0x39, 0x15, 0x01, 0x25, 0x08, 0x35, 0x00,
	0x46, 0x3b, 0x01, 0x66, 0x14, 0x00, 0x75, 0x04, 0x95, 0x01, 0x81, 0x42, 0x75, 0x04, 0x95, 0x01,
	0x15, 0x00, 0x25, 0x00, 0x35, 0x00, 0x45, 0x00, 0x65, 0x00, 0x81, 0x03, 0x05, 0x09, 0x19, 0x01,
	0x29, 0x0f, 0x15, 0x00, 0x25, 0x01, 0x75, 0x01, 0x95, 0x0f, 0x81, 0x02, 0x15, 0x00, 0x25, 0x00,
	0x75, 0x01, 0x95, 0x01, 0x81, 0x03, 0x05, 0x0c, 0x0a, 0xb2, 0x00, 0x15, 0x00, 0x25, 0x01, 0x95,
	0x01, 0x75, 0x01, 0x81, 0x02, 0x15, 0x00, 0x25, 0x00, 0x75, 0x07, 0x95, 0x01, 0x81, 0x03, 0x05,
	0x0f, 0x09, 0x21, 0x85, 0x03, 0xa1, 0x02, 0x09, 0x97, 0x15, 0x00, 0x25, 0x01, 0x75, 0x04, 0x95,
	0x01, 0x91, 0x02, 0x15, 0x00, 0x25, 0x00, 0x75, 0x04, 0x95, 0x01, 0x91, 0x03, 0x09, 0x70, 0x15,
	0x00, 0x25, 0x64, 0x75, 0x08, 0x95, 0x04, 0x91, 0x02, 0x09, 0x50, 0x66, 0x01, 0x10, 0x55, 0x0e,
	0x15, 0x00, 0x26, 0xff, 0x00, 0x75, 0x08, 0x95, 0x01, 0x91, 0x02, 0x09, 0xa7, 0x15, 0x00, 0x26,
	0xff, 0x00, 0x75, 0x08, 0x95, 0x01, 0x91, 0x02, 0x65, 0x00, 0x55, 0x00, 0x09, 0x7c, 0x15, 0x00,
	0x26, 0xff, 0x00, 0x75, 0x08, 0x95, 0x01, 0x91, 0x02, 0xc0, 0xc0,
}

// gpUhidBit — индекс кнопки Gamepad API (браузер, standard mapping) → номер бита
// в 15-битном поле кнопок HID-репорта. Раскладка Xbox: кнопки стоят на битах
// 0,1,3,4,6,7,10..14 (дыры на 2,5,8,9 — под несуществующие C/Z/TL2/TR2), чтобы
// generic HID→evdev маппинг ядра дал ровно BTN_A/B/X/Y/TL/TR/SELECT/START/MODE/
// THUMBL/THUMBR. Триггеры (6,7) — аналоговые оси, крестовина (12..15) — hat.
var gpUhidBit = map[int]uint{
	0: 0, 1: 1, // A, B
	2: 3, 3: 4, // X, Y
	4: 6, 5: 7, // LB, RB
	8: 10, 9: 11, // View(Back), Menu(Start)
	16: 12,         // Guide (Xbox)
	10: 13, 11: 14, // LS, RS
}

// uhidGamepad — состояние виртуального пада + fd на /dev/uhid. Состояние меняется
// под gpMu (внешний, вызывающий), запись в fd сериализуется wmu (send и ответы
// reader-горутины пишут в один fd).
type uhidGamepad struct {
	f   *os.File
	wmu sync.Mutex

	lx, ly, rx, ry uint16  // стики, 0..65535, центр 0x8000
	lt, rt         uint16  // триггеры, 0..1023
	btns           uint16  // 15 бит кнопок
	dpad           [4]bool // [0]=up [1]=down [2]=left [3]=right
	share          bool
}

// newUhidGamepad открывает /dev/uhid, создаёт HID-устройство по дескриптору Xbox
// и стартует reader-горутину (дренаж событий + ответы на GET/SET_REPORT). Ошибка
// (обычно EACCES — нет udev-правила uaccess на /dev/uhid) → вызывающий откатится
// на uinput.
func newUhidGamepad() (*uhidGamepad, error) {
	f, err := os.OpenFile("/dev/uhid", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	g := &uhidGamepad{f: f, lx: 0x8000, ly: 0x8000, rx: 0x8000, ry: 0x8000}

	// UHID_CREATE2: type(4) + name[128]@4 + phys[64]@132 + uniq[64]@196 +
	// rd_size u16@260 + bus u16@262 + vendor@264 + product@268 + version@272 +
	// country@276 + rd_data@280.
	buf := make([]byte, 280+len(gamepadHIDDesc))
	binary.LittleEndian.PutUint32(buf[0:], uhidCreate2)
	copy(buf[4:132], "Xbox Wireless Controller")
	binary.LittleEndian.PutUint16(buf[260:], uint16(len(gamepadHIDDesc)))
	binary.LittleEndian.PutUint16(buf[262:], busBluetooth)
	binary.LittleEndian.PutUint32(buf[264:], 0x045e) // vendor: Microsoft
	binary.LittleEndian.PutUint32(buf[268:], 0x0b13) // product: Xbox Wireless Controller
	binary.LittleEndian.PutUint32(buf[272:], 0x0001) // version
	copy(buf[280:], gamepadHIDDesc)
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return nil, fmt.Errorf("uhid create: %w", err)
	}

	go g.readLoop()
	g.send() // нейтральный старт-репорт (стики в центре)
	return g, nil
}

// readLoop дренирует события ядра и отвечает на GET/SET_REPORT (иначе драйвер
// может подвиснуть на запросе фичи при инициализации). Остальное игнорируем.
func (g *uhidGamepad) readLoop() {
	b := make([]byte, 4380) // sizeof(struct uhid_event)
	for {
		n, err := g.f.Read(b)
		if err != nil {
			return
		}
		if n < 8 {
			continue
		}
		switch binary.LittleEndian.Uint32(b[0:4]) {
		case uhidGetReport:
			id := binary.LittleEndian.Uint32(b[4:8])
			// UHID_GET_REPORT_REPLY: type@0 id@4 err u16@8 size u16@10 data@12.
			r := make([]byte, 12)
			binary.LittleEndian.PutUint32(r[0:], uhidGetReportReply)
			binary.LittleEndian.PutUint32(r[4:], id)
			g.write(r)
		case uhidSetReport:
			id := binary.LittleEndian.Uint32(b[4:8])
			// UHID_SET_REPORT_REPLY: type@0 id@4 err u16@8.
			r := make([]byte, 10)
			binary.LittleEndian.PutUint32(r[0:], uhidSetReportReply)
			binary.LittleEndian.PutUint32(r[4:], id)
			g.write(r)
		}
	}
}

func (g *uhidGamepad) write(b []byte) {
	g.wmu.Lock()
	_, _ = g.f.Write(b)
	g.wmu.Unlock()
}

// send собирает 17-байтный INPUT-репорт из текущего состояния и шлёт UHID_INPUT2.
func (g *uhidGamepad) send() {
	var r [17]byte
	r[0] = 0x01
	binary.LittleEndian.PutUint16(r[1:], g.lx)
	binary.LittleEndian.PutUint16(r[3:], g.ly)
	binary.LittleEndian.PutUint16(r[5:], g.rx)
	binary.LittleEndian.PutUint16(r[7:], g.ry)
	binary.LittleEndian.PutUint16(r[9:], g.lt)
	binary.LittleEndian.PutUint16(r[11:], g.rt)
	r[13] = g.hat()
	binary.LittleEndian.PutUint16(r[14:], g.btns)
	if g.share {
		r[16] = 1
	}
	// UHID_INPUT2: type u32@0 + size u16@4 + data@6.
	buf := make([]byte, 6+len(r))
	binary.LittleEndian.PutUint32(buf[0:], uhidInput2)
	binary.LittleEndian.PutUint16(buf[4:], uint16(len(r)))
	copy(buf[6:], r[:])
	g.write(buf)
}

// hat переводит состояние крестовины в HID hat switch (1=N по часовой до 8=NW,
// 0=нейтраль).
func (g *uhidGamepad) hat() byte {
	up, down, left, right := g.dpad[0], g.dpad[1], g.dpad[2], g.dpad[3]
	switch {
	case up && right:
		return 2
	case right && down:
		return 4
	case down && left:
		return 6
	case left && up:
		return 8
	case up:
		return 1
	case right:
		return 3
	case down:
		return 5
	case left:
		return 7
	}
	return 0
}

// button/axis обновляют состояние и шлют репорт. Вызываются под gpMu.
func (g *uhidGamepad) button(btn int, down bool, val float64) {
	switch {
	case btn == 6: // LT — аналоговый триггер
		g.lt = trig10(val)
	case btn == 7: // RT
		g.rt = trig10(val)
	case btn >= 12 && btn <= 15: // крестовина
		g.dpad[btn-12] = down
	default:
		bit, ok := gpUhidBit[btn]
		if !ok {
			return
		}
		if down {
			g.btns |= 1 << bit
		} else {
			g.btns &^= 1 << bit
		}
	}
	g.send()
}

func (g *uhidGamepad) axis(axis int, val float64) {
	u := stick16(val)
	switch axis {
	case 0:
		g.lx = u
	case 1:
		g.ly = u
	case 2:
		g.rx = u
	case 3:
		g.ry = u
	default:
		return
	}
	g.send()
}

func (g *uhidGamepad) close() {
	if g == nil || g.f == nil {
		return
	}
	r := make([]byte, 4)
	binary.LittleEndian.PutUint32(r[0:], uhidDestroy)
	g.write(r)
	g.f.Close()
}

// stick16 маппит ось Gamepad API (-1..1) в HID (0..65535, центр 0x8000). Для Y
// браузер даёт -1 вверх → 0, что совпадает с Xbox HID (верх = минимум).
func stick16(val float64) uint16 {
	v := (val + 1) / 2
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return uint16(v * 65535)
}

// trig10 маппит триггер (0..1) в 10-битный HID-диапазон (0..1023).
func trig10(val float64) uint16 {
	if val < 0 {
		val = 0
	}
	if val > 1 {
		val = 1
	}
	return uint16(val * 1023)
}
