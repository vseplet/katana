package main

import (
	"net/http"
	"strconv"

	"github.com/vseplet/katana/proto/capture"
)

// registerDisplayRoutes вешает GET /api/displays — список экранов avfoundation
// и индекс по умолчанию (флаг --screen), чтобы фронт выбрал монитор.
func registerDisplayRoutes(mux *http.ServeMux, defaultScreen int) {
	mux.HandleFunc("/api/displays", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"default": defaultScreen,
			"screens": capture.ListScreens(),
		})
	})

	// Источники ScreenCaptureKit: окна и приложения (для захвата окна/приложения).
	mux.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		sources, err := capture.ListSources()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, sources)
	})

	// Вывести приложение (по pid) на передний план на хосте.
	mux.HandleFunc("/api/activate", func(w http.ResponseWriter, r *http.Request) {
		pid, err := strconv.Atoi(r.URL.Query().Get("pid"))
		if err != nil {
			http.Error(w, "bad pid", http.StatusBadRequest)
			return
		}
		if err := capture.ActivateApp(pid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
