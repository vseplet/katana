package main

import "github.com/go-vgo/robotgo"

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

// scrollMouse прокручивает колесо: dx — горизонталь, dy — вертикаль (в «кликах»).
// msDelay=0 — без усыпляющей паузы (для непрерывного скролла свайпом).
func scrollMouse(dx, dy int) { robotgo.Scroll(dx, dy, 0) }
