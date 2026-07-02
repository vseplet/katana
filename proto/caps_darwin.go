//go:build darwin

package main

import "github.com/vseplet/katana/proto/permissions"

// hostCaps на macOS: видео/ввод зависят от выданных прав (Screen Recording /
// Accessibility), звук и терминал доступны всегда.
func hostCaps() capsInfo {
	return capsInfo{
		Video:    permissions.ScreenCaptureAllowed(),
		Audio:    true,
		Input:    permissions.AccessibilityAllowed(),
		Terminal: true,
	}
}
