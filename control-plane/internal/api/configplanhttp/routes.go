package configplanhttp

import "net/http"

// Guard authenticates and authorizes the declared action before next.
type Guard func(string, http.HandlerFunc) http.HandlerFunc

func (h *Handler) Register(mux *http.ServeMux, guard Guard) {
	if guard == nil {
		panic("configplanhttp: authorization guard is required")
	}
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/config-plans", guard("config.plan", h.createConfigPlan))
	mux.HandleFunc("GET /api/v1/config-plans/{plan_id}", guard("config.review", h.getConfigPlan))
	mux.HandleFunc("POST /api/v1/config-plans/{plan_id}/apply", guard("config.apply", h.applyConfigPlan))
}
