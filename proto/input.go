package main

import (
	"github.com/go-vgo/robotgo"

	"github.com/vseplet/katana/proto/capture"
)

// moveMouse перемещает курсор в глобальные координаты (точки, top-left).
func moveMouse(x, y int) { robotgo.Move(x, y) }

// mouseLocation возвращает текущую позицию курсора (глобальные точки) — чтобы
// хост сообщал её вьюеру (подсветка курсора и follow-pan при зуме на мобиле).
func mouseLocation() (int, int) { return robotgo.Location() }

// mouseToggle жмёт/отпускает кнопку мыши ("left" | "right" | "center").
func mouseToggle(button string, down bool) {
	state := "up"
	if down {
		state = "down"
	}
	robotgo.Toggle(button, state)
}

// dragMouse постит событие перетаскивания (мышь с зажатой кнопкой) в (x, y).
func dragMouse(x, y int, button string) { robotgo.Drag(x, y, button) }

// moveRel сдвигает курсор на (dx, dy) от текущей позиции — трекпад-режим мобилы
// (свайп = относительное движение, а не абсолютное позиционирование).
func moveRel(dx, dy int) {
	x, y := robotgo.Location()
	robotgo.Move(x+dx, y+dy)
}

// clickMouse кликает кнопкой ("left"|"right") по ТЕКУЩЕЙ позиции курсора (тап).
func clickMouse(button string) { robotgo.Click(button) }

// doubleClick — двойной клик по текущей позиции (правильный clickCount=2, чтобы
// ОС распознала double — например, разворот окна по заголовку).
func doubleClick(button string) { robotgo.Click(button, true) }

// dragRel тащит на (dx, dy) от текущей позиции с зажатой кнопкой (drag-событие,
// не просто move) — относительное перетаскивание (long-press-drag на мобиле).
func dragRel(dx, dy int, button string) {
	x, y := robotgo.Location()
	robotgo.Drag(x+dx, y+dy, button)
}

// scrollMouse прокручивает на dx/dy ПИКСЕЛЕЙ (пиксельно-точно, как трекпад) —
// нативный CGEvent с пиксельными единицами, а не строчный robotgo.Scroll.
func scrollMouse(dx, dy int) { capture.InjectScroll(dx, dy) }

// tapKey нажимает клавишу key с модификаторами mods ("ctrl"|"alt"|"cmd"|"shift").
// Для спец-клавиш (enter/tab/стрелки/…) и шорткатов (Cmd+C и т.п.).
func tapKey(key string, mods []string) {
	args := make([]interface{}, len(mods))
	for i, m := range mods {
		args[i] = m
	}
	robotgo.KeyTap(key, args...)
}

// typeText печатает произвольный текст (символы/регистр/юникод) — для обычного
// набора (TypeStr корректно обрабатывает shift-символы, в отличие от KeyTap).
func typeText(s string) { robotgo.TypeStr(s) }
