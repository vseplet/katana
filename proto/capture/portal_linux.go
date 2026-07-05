//go:build linux

package capture

// Сессия xdg-desktop-portal для Wayland: ЧИСТЫЙ ScreenCast (только захват экрана,
// без RemoteDesktop). Ввод (мышь/клавиатура) идёт мимо портала — через uinput
// (см. proto/input_linux.go), поэтому сессия не содержит удалённого управления.
// Это принципиально: KDE отказывается персистить сессии с управлением вводом
// («Remote desktop sessions cannot persist»), а чистый ScreenCast — персистит.
// Поэтому здесь используем persist_mode=2 + restore_token: диалог KDE показывается
// ОДИН раз, дальше грант восстанавливается по токену (переживает рестарт,
// перезагрузку и обновление SteamOS — токен лежит на /home). Сессия — синглтон.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/godbus/dbus/v5"
)

type portalSession struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	session dbus.ObjectPath
	node    uint32 // ScreenCast-поток (PipeWire node id для захвата)
}

var (
	portalMu   sync.Mutex
	portalSess *portalSession
	portalErr  error // кэш отказа/ошибки — чтобы не долбить диалогом повторно
)

// --- restore_token: персист гранта захвата между запусками ---------------------
// Файл в $XDG_STATE_HOME (по умолчанию ~/.local/state) — на SteamOS это отдельный
// раздел /home, переживающий перезагрузку и обновление системы.

func tokenPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "katana", "screencast.token")
}

func loadToken() string {
	p := tokenPath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func saveToken(tok string) {
	p := tokenPath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(tok), 0o600)
}

// ensurePortal лениво создаёт ScreenCast-сессию (один раз). При отказе пользователя
// кэширует ошибку (до перезапуска хоста).
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

	// 1) ScreenCast.CreateSession
	res, err := portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.CreateSession",
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

	// 2) ScreenCast.SelectSources: монитор, курсор в кадре, persist до отзыва.
	// restore_token добавляем ТОЛЬКО если он валиден (KDE отвергает пустую строку:
	// «Restore token is not a valid UUID string»). На первом запуске шлём лишь
	// persist_mode — KDE покажет диалог и вернёт токен в результате Start.
	selOpts := map[string]dbus.Variant{
		"types":        dbus.MakeVariant(uint32(1)), // MONITOR
		"cursor_mode":  dbus.MakeVariant(uint32(2)), // EMBEDDED (курсор в кадре)
		"multiple":     dbus.MakeVariant(false),
		"persist_mode": dbus.MakeVariant(uint32(2)), // persist until revoked
	}
	if tok := loadToken(); tok != "" {
		selOpts["restore_token"] = dbus.MakeVariant(tok)
	}
	if _, err := portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.SelectSources",
		selOpts, session); err != nil {
		conn.Close()
		return nil, err
	}

	// 3) ScreenCast.Start → диалог KDE (первый раз), возвращает streams + токен.
	res, err = portalRequest(conn, obj, "org.freedesktop.portal.ScreenCast.Start",
		map[string]dbus.Variant{}, session, "")
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Сохраняем restore_token (портал мог его ротировать) для следующего запуска.
	if v, ok := res["restore_token"]; ok {
		if tok, _ := v.Value().(string); tok != "" {
			saveToken(tok)
			log.Printf("portal: restore_token saved (screencast persist)")
		}
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
