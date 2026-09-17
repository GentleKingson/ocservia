package useroperationshttp

import "net/http"

type Guard func(string, http.HandlerFunc) http.HandlerFunc

func (h *Handler) Register(mux *http.ServeMux, guard Guard) {
	if guard == nil {
		panic("useroperationshttp: authorization guard is required")
	}
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/users/{username}/policy", guard("node.read", h.getUserPolicy))
	mux.HandleFunc("PUT /api/v1/nodes/{node_id}/users/{username}/policy", guard("user.manage", h.setUserPolicy))
	mux.HandleFunc("POST /api/v1/user-batches", guard("user.manage", h.createUserBatch))
	mux.HandleFunc("GET /api/v1/user-batches/{batch_id}", guard("operation.read", h.getUserBatch))
	mux.HandleFunc("GET /api/v1/user-operations/metrics", guard("operation.read", h.userOperationMetrics))
}
