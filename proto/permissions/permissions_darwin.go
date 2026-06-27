//go:build darwin

// Package permissions запрашивает и проверяет macOS-разрешения TCC:
// захват экрана (Screen Recording) и управление вводом (Accessibility).
package permissions

/*
#cgo LDFLAGS: -framework CoreGraphics -framework ApplicationServices -framework CoreFoundation
#include <stdbool.h>
#include <CoreFoundation/CoreFoundation.h>
#include <ApplicationServices/ApplicationServices.h>

// Объявлены в CoreGraphics (macOS 10.15+). Форвард-декларации на случай,
// если umbrella-заголовок их не подтянул.
extern bool CGPreflightScreenCaptureAccess(void);
extern bool CGRequestScreenCaptureAccess(void);

static int screenPreflight(void) {
	return CGPreflightScreenCaptureAccess() ? 1 : 0;
}

static int screenRequest(void) {
	return CGRequestScreenCaptureAccess() ? 1 : 0;
}

// axTrusted проверяет доступ к Accessibility; при prompt=1 показывает системный
// диалог с предложением выдать доступ (один раз на приложение).
static int axTrusted(int prompt) {
	const void *keys[] = { kAXTrustedCheckOptionPrompt };
	const void *vals[] = { prompt ? kCFBooleanTrue : kCFBooleanFalse };
	CFDictionaryRef opts = CFDictionaryCreate(
		NULL, keys, vals, 1,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	Boolean t = AXIsProcessTrustedWithOptions(opts);
	CFRelease(opts);
	return t ? 1 : 0;
}
*/
import "C"

// ScreenCaptureAllowed возвращает текущий статус разрешения на запись экрана
// (без показа диалога).
func ScreenCaptureAllowed() bool { return C.screenPreflight() == 1 }

// RequestScreenCapture показывает системный диалог запроса записи экрана
// (только при первом обращении) и возвращает актуальный статус.
func RequestScreenCapture() bool { return C.screenRequest() == 1 }

// AccessibilityAllowed возвращает статус доступа к Accessibility (нужен для
// инъекции мыши/клавиатуры) без показа диалога.
func AccessibilityAllowed() bool { return C.axTrusted(0) == 1 }

// RequestAccessibility проверяет доступ и при отсутствии показывает системный
// диалог (один раз на приложение).
func RequestAccessibility() bool { return C.axTrusted(1) == 1 }
