package nodehttp

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
)

// Guard authenticates and authorizes the server-declared action before next.
type Guard func(action string, next http.HandlerFunc) http.HandlerFunc

// Register installs the five reads on the application's root mux.
func (h *Handler) Register(mux httpx.Registrar, guard Guard) {
	if guard == nil {
		panic("nodehttp: authorization guard is required")
	}
	mux.HandleFunc("GET /api/v1/nodes", guard("node.read", h.listNodes))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}", guard("node.read", h.getNode))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/sessions", guard("node.read", h.listNodeSessions))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/ip-bans", guard("node.read", h.listNodeIPBans))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/telemetry", guard("node.read", h.listNodeTelemetry))
}
