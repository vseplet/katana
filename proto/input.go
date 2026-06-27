package main

import (
	"github.com/go-vgo/robotgo"

	"github.com/vseplet/katana/proto/capture"
)

// moveMouse перемещает курсор в глобальные координаты (точки, top-left).
func moveMouse(x, y int) { robotgo.Move(x, y) }

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

// scrollMouse прокручивает на dx/dy ПИКСЕЛЕЙ (пиксельно-точно, как трекпад) —
// нативный CGEvent с пиксельными единицами, а не строчный robotgo.Scroll.
func scrollMouse(dx, dy int) { capture.InjectScroll(dx, dy) }
