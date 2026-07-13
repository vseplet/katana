//go:build !(windows && cgo && winnative)

// Заглушка для не-winnative сборок: пакет всегда имеет собираемый main, чтобы
// `go build ./...` не падал на платформах без AMF (mac/linux/обычная Windows-сборка).
package main

func main() {}
