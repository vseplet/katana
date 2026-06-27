package main

import (
	"net/http"

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
}
