//go:build !darwin

// На не-macOS нет TCC (Screen Recording / Accessibility). Захвата экрана в
// terminal-only сборке нет — поэтому screen-права всегда false; клавиатура/мышь
// (Accessibility) тоже неприменимы. Заглушки, чтобы пакет собирался под Linux.
package permissions

func ScreenCaptureAllowed() bool { return false }
func RequestScreenCapture() bool { return false }
func AccessibilityAllowed() bool { return false }
func RequestAccessibility() bool { return false }
