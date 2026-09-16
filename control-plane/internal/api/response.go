package api

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
)

func writeProblem(w http.ResponseWriter, r *http.Request, status int, kind, title, detail string) {
	httpx.WriteProblem(w, r, status, kind, title, detail)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	httpx.WriteJSON(w, status, value)
}
