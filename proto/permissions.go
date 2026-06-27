package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os/exec"

	"github.com/vseplet/katana/proto/permissions"
)

// permStatus — статус TCC-разрешений для фронта.
type permStatus struct {
	Screen        bool `json:"screen"`        // запись экрана (Screen Recording)
	Accessibility bool `json:"accessibility"` // управление вводом (для remote control)
}

// registerPermissionRoutes вешает API разрешений:
//
//	GET  /api/permissions                 — текущий статус
//	POST /api/permissions/screen          — запросить запись экрана (диалог)
//	POST /api/permissions/accessibility   — запросить Accessibility (диалог)
//	POST /api/permissions/open?target=... — открыть нужную панель System Settings
func registerPermissionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/permissions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, permStatus{
			Screen:        permissions.ScreenCaptureAllowed(),
			Accessibility: permissions.AccessibilityAllowed(),
		})
	})

	mux.HandleFunc("/api/permissions/screen", func(w http.ResponseWriter, r *http.Request) {
		granted := permissions.RequestScreenCapture()
		log.Printf("permissions: screen recording requested -> %v", granted)
		writeJSON(w, permStatus{Screen: granted, Accessibility: permissions.AccessibilityAllowed()})
	})

	mux.HandleFunc("/api/permissions/accessibility", func(w http.ResponseWriter, r *http.Request) {
		granted := permissions.RequestAccessibility()
		log.Printf("permissions: accessibility requested -> %v", granted)
		writeJSON(w, permStatus{Screen: permissions.ScreenCaptureAllowed(), Accessibility: granted})
	})

	// Запрос-диалог показывается системой лишь однажды; если уже отказано —
	// пользователю надо включить вручную. Открываем нужную панель настроек.
	mux.HandleFunc("/api/permissions/open", func(w http.ResponseWriter, r *http.Request) {
		var url string
		switch r.URL.Query().Get("target") {
		case "accessibility":
			url = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
		default:
			url = "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
		}
		if err := exec.Command("open", url).Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
