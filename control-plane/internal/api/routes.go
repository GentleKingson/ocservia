package api

import (
	"net/http"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/httpx"
)

func (s *Server) registerRoutes(mux *http.ServeMux) []moduleMethodRule {
	modules := &moduleRegistrar{mux: mux}
	s.registerHealthRoutes(mux)
	s.registerAuthRoutes(mux)
	s.registerDevelopmentRoutes(mux)
	s.registerOperationsRoutes(mux)
	s.registerEnrollmentRoutes(mux)
	s.registerNodeRoutes(modules)
	s.registerUserRoutes(mux, modules)
	s.registerConfigPlanRoutes(modules)
	s.registerCertificateRoutes(mux)
	s.registerAuthorizationRoutes(mux)
	return modules.rules
}

func (s *Server) registerHealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /livez", s.live)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /api/v1/livez", s.live)
	mux.HandleFunc("GET /api/v1/readyz", s.ready)
	mux.HandleFunc("GET /api/v1/version", s.version)
}

func (s *Server) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/login", s.limitAuthentication(newAuthAdmission(30, 120, 8), s.login))
	mux.HandleFunc("POST /api/v1/auth/login", s.localLogin)
	mux.HandleFunc("GET /api/v1/auth/methods", s.authMethods)
	mux.HandleFunc("GET /api/v1/auth/callback", s.limitAuthentication(newAuthAdmission(30, 120, 8), s.callback))
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireOperationAuth(s.logout))
	mux.HandleFunc("POST /api/v1/auth/change-password", s.requireOperationAuth(s.changeLocalPassword))
	mux.HandleFunc("POST /api/v1/local-users", s.requireOperationAuth(s.createLocalUser))
	mux.HandleFunc("POST /api/v1/local-users/{local_user_action}", s.requireOperationAuth(s.localUserAction))
	mux.HandleFunc("POST /api/v1/auth/break-glass", s.breakGlass)
}

func (s *Server) registerDevelopmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/development/simulations", s.createSimulation)
	mux.HandleFunc("GET /api/v1/development/runtime", s.developmentRuntime)
}

func (s *Server) registerOperationsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/operations", s.requireOperationAuth(s.listOperations))
	mux.HandleFunc("GET /api/v1/operations/summary", s.requireOperationAuth(s.operationSummary))
	mux.HandleFunc("GET /api/v1/operations/{operation_id}", s.requireOperationAuth(s.getOperation))
	mux.HandleFunc("GET /api/v1/operations/{operation_id}/events", s.requireOperationAuth(s.streamOperationEvents))
	mux.HandleFunc("GET /api/v1/operations/queue-metrics", s.requireOperationAuth(s.queueMetrics))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/synthetic-commands", s.requireOperationAuth(s.createSyntheticCommand))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/sessions/{session_action}", s.requireOperationAuth(s.sessionAction))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/ip-bans/{ip_action}", s.requireOperationAuth(s.ipBanAction))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/service:reload", s.requireOperationAuth(s.reloadService))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/agent-upgrade", s.requireOperationAuth(s.upgradeAgent))
	mux.HandleFunc("GET /api/v1/events", s.requireOperationAuth(s.listEvents))
	mux.HandleFunc("GET /api/v1/events/stream", s.requireOperationAuth(s.streamEvents))
	mux.HandleFunc("POST /api/v1/agent-rollouts", s.requireOperationAuth(s.createAgentRollout))
	mux.HandleFunc("GET /api/v1/agent-rollouts", s.requireOperationAuth(s.listAgentRollouts))
	mux.HandleFunc("GET /api/v1/agent-rollouts/{rollout_id}", s.requireOperationAuth(s.getAgentRollout))
	mux.HandleFunc("POST /api/v1/agent-rollouts/{rollout_id}/resume", s.requireOperationAuth(s.resumeAgentRollout))
}

func (s *Server) registerEnrollmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/enrollment-tokens", s.requireOperationAuth(s.createEnrollmentToken))
	mux.HandleFunc("POST /api/v1/node-bootstrap-tokens", s.requireOperationAuth(s.createNodeBootstrapToken))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/approval", s.requireOperationAuth(s.approveNode))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/revocation", s.requireOperationAuth(s.revokeNode))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-credentials", s.requireOperationAuth(s.createPrivdAttestationCredential))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-keys:register", s.registerPrivdAttestationKey)
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-keys:revoke", s.requireOperationAuth(s.revokePrivdAttestationKey))
}

func (s *Server) registerNodeRoutes(mux httpx.Registrar) {
	s.nodeHTTP.Register(mux, s.requireActionAuth)
}

func (s *Server) registerUserRoutes(mux *http.ServeMux, modules httpx.Registrar) {
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/user-group-state", s.requireOperationAuth(s.listUserGroupState))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/users", s.requireOperationAuth(s.createUser))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/users/{user_action}", s.requireOperationAuth(s.userAction))
	mux.HandleFunc("PUT /api/v1/nodes/{node_id}/groups/{group_name}", s.requireOperationAuth(s.applyGroup))
	s.userOpsHTTP.Register(modules, s.requireActionAuth)
}

func (s *Server) registerConfigPlanRoutes(mux httpx.Registrar) {
	s.configPlanHTTP.Register(mux, s.requireActionAuth)
}

func (s *Server) registerCertificateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.createCertificate))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.listNodeCertificates))
	mux.HandleFunc("GET /api/v1/certificates/{certificate_id}", s.requireOperationAuth(s.getCertificate))
	mux.HandleFunc("POST /api/v1/certificates/{certificate_action}", s.requireOperationAuth(s.certificateAction))
	mux.HandleFunc("GET /api/v1/artifacts/{artifact_id}", s.requireOperationAuth(s.downloadArtifact))
	mux.HandleFunc("POST /api/v1/secret-provider-refs", s.requireOperationAuth(s.createSecretRef))
	mux.HandleFunc("GET /api/v1/secret-provider-refs/{secret_ref_id}", s.requireOperationAuth(s.getSecretRef))
	mux.HandleFunc("POST /api/v1/secret-provider-refs/{secret_ref_action}", s.requireOperationAuth(s.rotateSecretRef))
}

func (s *Server) registerAuthorizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/approval-requests", s.requireOperationAuth(s.createApproval))
	mux.HandleFunc("GET /api/v1/approval-requests/{approval_id}", s.requireOperationAuth(s.getApproval))
	mux.HandleFunc("POST /api/v1/approval-requests/{approval_id}", s.requireOperationAuth(s.approveRequest))
	mux.HandleFunc("GET /api/v1/audit/events", s.requireOperationAuth(s.listAuditEvents))
	mux.HandleFunc("POST /api/v1/audit:verify", s.requireOperationAuth(s.verifyAudit))
	mux.HandleFunc("GET /api/v1/workspaces", s.requireOperationAuth(s.listWorkspaces))
	mux.HandleFunc("POST /api/v1/role-bindings", s.requireOperationAuth(s.createRoleBinding))
}
