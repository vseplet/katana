package main

// capsInfo — что хост реально может предоставить в текущем окружении. Считается в
// рантайме (hostCaps, платформо-зависимо), показывается в TUI чекбоксами и
// уходит вьюверу в hostinfo, чтобы тот адаптировался (нет видео — не ждёт его).
type capsInfo struct {
	Video    bool // захват экрана доступен
	Audio    bool // передача звука доступна
	Input    bool // управление мышью/клавиатурой доступно
	Terminal bool // общий терминал доступен (обычно всегда)
	Gamepad  bool // виртуальный геймпад доступен (uinput на Linux)
	// MouseCapture — есть сырой relative-ввод для захвата мыши в играх (uinput на
	// Linux). Вьювер по флагу включает Pointer Lock. См. docs/mouse-capture.md.
	MouseCapture bool
}
