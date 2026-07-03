//go:build linux

package capture

// Единая сессия xdg-desktop-portal для Wayland: RemoteDesktop (инъекция мыши/
// клавиатуры) + ScreenCast (захват экрана) в ОДНОМ сеансе — один диалог KDE
// «разрешить удалённое управление и захват экрана». Абсолютная позиция курсора
// идёт через NotifyPointerMotionAbsolute (истинные координаты через композитор,
// без uinput-ускорения). Сессия — синглтон на весь процесс хоста.

import (
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

type portalSession struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	session dbus.ObjectPath
	node    uint32 // ScreenCast-поток (для маппинга абсолютных координат мыши)
}

var (
	portalMu   sync.Mutex
	portalSess *portalSession
	portalErr  error // кэш отказа/ошибки — чтобы не долбить диалогом повторно
)

// ensurePortal лениво создаёт объединённую RemoteDesktop+ScreenCast сессию (один
// раз). При отказе пользователя кэширует ошибку (до перезапуска хоста).
func ensurePortal() (*portalSession, error) {
	portalMu.Lock()
	defer portalMu.Unlock()
	if portalSess != nil {
		return portalSess, nil
	}
	if portalErr != nil {
		return nil, portalErr
	}
	ps, err := openPortal()
	if err != nil {
		portalErr = err
		return nil, err
	}
	portalSess = ps
	return ps, nil
}

func openPortal() (*portalSession, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	obj := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	// 1) RemoteDesktop.CreateSession
	res, err := portalRequest(conn, obj, "org.freedesktop.portal.RemoteDesktop.CreateSession",
		map[string]dbus.Variant{"session_handle_token": dbus.MakeVariant(newHandleToken())})
	if err != nil {
		conn.Close()
		return nil, err
	}
	sh := sessionHandleOf(res)
	if sh == "" {
		conn.Close()
		return nil, fmt.Errorf("портал не вернул session_handle")
	}
	session := dbus.ObjectPath(sh)

	// 2) RemoteDesktop.SelectDevices: клавиатура(1)+указатель(2) = 3
	if _, err := portalRequest(conn, obj, "org.freedesktop.portal.RemoteDesktop.SelectDevices",
		map[string]dbus.Variant{"types": dbus.MakeVariant(uint32(3))}, session); err != nil {
		conn.Close()
		return nil, err
	}

	// 3) ScreenCast.SelectSources в ТОЙ ЖЕ сессии: монитор, курсор в кадре
	if _, err := portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.SelectSources",
		map[string]dbus.Variant{
			"types":       dbus.MakeVariant(uint32(1)), // MONITOR
			"cursor_mode": dbus.MakeVariant(uint32(2)), // EMBEDDED
			"multiple":    dbus.MakeVariant(false),
		}, session); err != nil {
		conn.Close()
		return nil, err
	}

	// 4) RemoteDesktop.Start → диалог KDE (экран + управление), возвращает streams
	res, err = portalRequest(conn, obj, "org.freedesktop.portal.RemoteDesktop.Start",
		map[string]dbus.Variant{}, session, "")
	if err != nil {
		conn.Close()
		return nil, err
	}
	var streams []struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	if err := dbus.Store([]interface{}{res["streams"].Value()}, &streams); err != nil || len(streams) == 0 {
		conn.Close()
		return nil, fmt.Errorf("нет ScreenCast-потоков: %v", err)
	}
	return &portalSession{conn: conn, obj: obj, session: session, node: streams[0].Node}, nil
}

// openPipeWire открывает свежий PipeWire-fd для видео (можно звать многократно).
func (p *portalSession) openPipeWire() (int, error) {
	var fd dbus.UnixFD
	if err := p.obj.Call("org.freedesktop.portal.ScreenCast.OpenPipeWireRemote", 0,
		p.session, map[string]dbus.Variant{}).Store(&fd); err != nil {
		return 0, fmt.Errorf("OpenPipeWireRemote: %w", err)
	}
	return int(fd), nil
}

var noOpts = map[string]dbus.Variant{}

func (p *portalSession) motionAbs(x, y float64) {
	_ = p.obj.Call("org.freedesktop.portal.RemoteDesktop.NotifyPointerMotionAbsolute", 0,
		p.session, noOpts, p.node, x, y).Err
}

func (p *portalSession) motionRel(dx, dy float64) {
	_ = p.obj.Call("org.freedesktop.portal.RemoteDesktop.NotifyPointerMotion", 0,
		p.session, noOpts, dx, dy).Err
}

func (p *portalSession) button(btn int32, down bool) {
	st := uint32(0)
	if down {
		st = 1
	}
	_ = p.obj.Call("org.freedesktop.portal.RemoteDesktop.NotifyPointerButton", 0,
		p.session, noOpts, btn, st).Err
}

func (p *portalSession) axis(dx, dy float64) {
	_ = p.obj.Call("org.freedesktop.portal.RemoteDesktop.NotifyPointerAxis", 0,
		p.session, noOpts, dx, dy).Err
}

func (p *portalSession) keycode(code int32, down bool) {
	st := uint32(0)
	if down {
		st = 1
	}
	_ = p.obj.Call("org.freedesktop.portal.RemoteDesktop.NotifyKeyboardKeycode", 0,
		p.session, noOpts, code, st).Err
}

// --- Экспортируемый API для main-пакета (ввод на Wayland через портал) ---

// PortalInputReady — готова ли портал-сессия принимать ввод (без попытки создать).
func PortalInputReady() bool {
	portalMu.Lock()
	defer portalMu.Unlock()
	return portalSess != nil
}

// PortalPointerMotion — абсолютная позиция курсора (пиксели потока = экрана).
func PortalPointerMotion(x, y float64) {
	if p, err := ensurePortal(); err == nil {
		p.motionAbs(x, y)
	}
}

// PortalPointerMotionRel — относительное движение (трекпад-режим мобилы).
func PortalPointerMotionRel(dx, dy float64) {
	if p, err := ensurePortal(); err == nil {
		p.motionRel(dx, dy)
	}
}

// PortalPointerButton — кнопка мыши (btn — evdev-код BTN_LEFT/RIGHT/MIDDLE).
func PortalPointerButton(btn int32, down bool) {
	if p, err := ensurePortal(); err == nil {
		p.button(btn, down)
	}
}

// PortalPointerAxis — прокрутка (dx/dy).
func PortalPointerAxis(dx, dy float64) {
	if p, err := ensurePortal(); err == nil {
		p.axis(dx, dy)
	}
}

// PortalKeyboard — клавиша (code — evdev keycode).
func PortalKeyboard(code int32, down bool) {
	if p, err := ensurePortal(); err == nil {
		p.keycode(code, down)
	}
}
