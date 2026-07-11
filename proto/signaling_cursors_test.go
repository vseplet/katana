package main

import (
	"testing"
	"time"
)

// idsOf — множество id в списке курсоров (для проверки состава без учёта порядка).
func idsOf(cs []peerCursor) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.ID] = true
	}
	return m
}

func TestPlanCursorBroadcast(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fresh := now.Add(-100 * time.Millisecond)
	stale := now.Add(-2 * time.Second)

	t.Run("каждый видит остальных, но не себя", func(t *testing.T) {
		states := []cursorState{
			{id: "a", active: true, ts: fresh, x: .1, y: .1},
			{id: "b", active: true, ts: fresh, x: .2, y: .2},
			{id: "c", active: true, ts: fresh, x: .3, y: .3},
		}
		send, had := planCursorBroadcast(states, now)
		for _, id := range []string{"a", "b", "c"} {
			cs, ok := send[id]
			if !ok {
				t.Fatalf("%s: ожидали рассылку", id)
			}
			if idsOf(cs)[id] {
				t.Errorf("%s: в своём списке есть он сам", id)
			}
			if len(cs) != 2 {
				t.Errorf("%s: ждали 2 чужих курсора, получили %d", id, len(cs))
			}
			if !had[id] {
				t.Errorf("%s: hadPeers должен стать true", id)
			}
		}
	})

	t.Run("протухшие и неактивные отсеиваются", func(t *testing.T) {
		states := []cursorState{
			{id: "a", active: true, ts: fresh, x: .1, y: .1},
			{id: "b", active: true, ts: stale, x: .2, y: .2}, // протух
			{id: "c", active: false, ts: fresh, x: .3, y: .3}, // ушёл с видео
		}
		send, _ := planCursorBroadcast(states, now)
		// a активен и свеж → b и c должны его видеть; сам a не видит никого (b,c мертвы).
		if got := idsOf(send["b"]); !got["a"] || len(got) != 1 {
			t.Errorf("b должен видеть только a, получил %v", got)
		}
		if got := idsOf(send["c"]); !got["a"] || len(got) != 1 {
			t.Errorf("c должен видеть только a, получил %v", got)
		}
		if _, ok := send["a"]; ok {
			t.Errorf("a: слать нечего (b,c мертвы) и раньше не слали → не должно быть рассылки")
		}
	})

	t.Run("пустой список шлётся ровно один раз (гейт hadPeers)", func(t *testing.T) {
		// Был активен, стал неактивен, но в прошлый тик получал курсоры → нужно
		// разово прислать пустой список, чтобы вьювер убрал чужие иконки.
		states := []cursorState{
			{id: "a", active: false, ts: fresh, hadPeers: true},
			{id: "b", active: false, ts: fresh, hadPeers: false},
		}
		send, had := planCursorBroadcast(states, now)
		cs, ok := send["a"]
		if !ok || len(cs) != 0 {
			t.Errorf("a: ждали разовый пустой список, got ok=%v cs=%v", ok, cs)
		}
		if had["a"] {
			t.Errorf("a: hadPeers должен сброситься в false")
		}
		if _, ok := send["b"]; ok {
			t.Errorf("b: и слать нечего, и раньше не слали → молчим")
		}
	})

	t.Run("меньше двух зрителей — призрак гасится, спама нет", func(t *testing.T) {
		// Остался один зритель, у которого висел чужой курсор (hadPeers=true) —
		// шлём пустой один раз (гасим призрак), дальше молчим.
		clear, _ := planCursorBroadcast(
			[]cursorState{{id: "a", active: true, ts: fresh, hadPeers: true}}, now)
		if cs, ok := clear["a"]; !ok || len(cs) != 0 {
			t.Errorf("одинокий зритель с призраком: ждали разовый пустой, got ok=%v cs=%v", ok, cs)
		}
		silent, _ := planCursorBroadcast(
			[]cursorState{{id: "a", active: true, ts: fresh, hadPeers: false}}, now)
		if _, ok := silent["a"]; ok {
			t.Errorf("одинокий зритель без призрака: рассылки быть не должно")
		}
	})
}

func TestCursorColorStable(t *testing.T) {
	if cursorColor("abc") != cursorColor("abc") {
		t.Error("цвет должен быть детерминирован по id")
	}
	c := cursorColor("some-viewer-id")
	found := false
	for _, p := range cursorPalette {
		if p == c {
			found = true
		}
	}
	if !found {
		t.Errorf("цвет %q вне палитры", c)
	}
}
