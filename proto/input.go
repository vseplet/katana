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
