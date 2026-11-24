package api

func (s *Server) registerRoutes(routes *methodRegistrar) {
	s.registerHealthRoutes(routes)
	s.registerAuthRoutes(routes)
	s.registerDevelopmentRoutes(routes)
	s.registerOperationsRoutes(routes)
	s.registerEnrollmentRoutes(routes)
	s.registerNodeRoutes(routes)
	s.registerUserRoutes(routes)
	s.registerConfigPlanRoutes(routes)
	s.registerCertificateRoutes(routes)
	s.registerAuthorizationRoutes(routes)
}

func (s *Server) registerHealthRoutes(routes *methodRegistrar) {
	routes.HandleFunc("GET /livez", s.live)
	routes.HandleFunc("GET /readyz", s.ready)
	routes.HandleFunc("GET /version", s.version)
	routes.HandleFunc("GET /api/v1/livez", s.live)
	routes.HandleFunc("GET /api/v1/readyz", s.ready)
	routes.HandleFunc("GET /api/v1/version", s.version)
}

func (s *Server) registerAuthRoutes(routes *methodRegistrar) {
	routes.HandleFunc("POST /api/v1/auth/login", s.localLogin)
	routes.HandleFunc("POST /api/v1/auth/logout", s.requireOperationAuth(s.logout))
	routes.HandleFunc("POST /api/v1/auth/change-password", s.requireOperationAuth(s.changeLocalPassword))
	routes.HandleFunc("POST /api/v1/auth/break-glass", s.breakGlass)
}

func (s *Server) registerDevelopmentRoutes(routes *methodRegistrar) {
	routes.HandleFunc("POST /api/v1/development/simulations", s.createSimulation)
	routes.HandleFunc("GET /api/v1/development/runtime", s.developmentRuntime)
}

func (s *Server) registerOperationsRoutes(routes *methodRegistrar) {
	routes.HandleFunc("GET /api/v1/operations", s.requireOperationAuth(s.listOperations))
	routes.HandleFunc("GET /api/v1/operations/summary", s.requireOperationAuth(s.operationSummary))
	routes.HandleFunc("GET /api/v1/operations/{operation_id}", s.requireOperationAuth(s.getOperation))
	routes.HandleFunc("GET /api/v1/operations/{operation_id}/events", s.requireOperationAuth(s.streamOperationEvents))
	routes.HandleFunc("GET /api/v1/operations/queue-metrics", s.requireOperationAuth(s.queueMetrics))
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/synthetic-commands", s.requireOperationAuth(s.createSyntheticCommand))
	routes.action("/api/v1/nodes/{node_id}/sessions/{session_action}", "session_action", nil, s.requireOperationAuth(s.sessionAction), "disconnect", "terminate")
	routes.action("/api/v1/nodes/{node_id}/ip-bans/{ip_action}", "ip_action", nil, s.requireOperationAuth(s.ipBanAction), "remove")
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/service:reload", s.requireOperationAuth(s.reloadService))
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/agent-upgrade", s.requireOperationAuth(s.upgradeAgent))
	routes.HandleFunc("GET /api/v1/events", s.requireOperationAuth(s.listEvents))
	routes.HandleFunc("GET /api/v1/events/stream", s.requireOperationAuth(s.streamEvents))
	routes.HandleFunc("POST /api/v1/agent-rollouts", s.requireOperationAuth(s.createAgentRollout))
	routes.HandleFunc("GET /api/v1/agent-rollouts", s.requireOperationAuth(s.listAgentRollouts))
	routes.HandleFunc("GET /api/v1/agent-rollouts/{rollout_id}", s.requireOperationAuth(s.getAgentRollout))
	routes.HandleFunc("POST /api/v1/agent-rollouts/{rollout_id}/resume", s.requireOperationAuth(s.resumeAgentRollout))
}

func (s *Server) registerEnrollmentRoutes(routes *methodRegistrar) {
	routes.HandleFunc("POST /api/v1/node-bootstrap/config", s.installationConfig)
	routes.HandleFunc("POST /api/v1/node-bootstrap/attestation", s.installationAttestation)
	routes.HandleFunc("POST /api/v1/node-bootstrap/register", s.installationRegister)
	routes.HandleFunc("POST /api/v1/node-bootstrap/status", s.installationStatus)
	routes.HandleFunc("POST /api/v1/node-bootstrap-tokens", s.requireOperationAuth(s.createNodeBootstrapToken))
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/revocation", s.requireOperationAuth(s.revokeNode))
}

func (s *Server) registerNodeRoutes(routes *methodRegistrar) {
	routes.HandleFunc("GET /api/v1/nodes", s.requireOperationAuth(s.listNodes))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}", s.requireOperationAuth(s.getNode))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/sessions", s.requireOperationAuth(s.listNodeSessions))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/ip-bans", s.requireOperationAuth(s.listNodeIPBans))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/telemetry", s.requireOperationAuth(s.listNodeTelemetry))
}

func (s *Server) registerUserRoutes(routes *methodRegistrar) {
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/user-password-sealing-key", s.requireOperationAuth(s.getUserPasswordSealingKey))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/user-group-state", s.requireOperationAuth(s.listUserGroupState))
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/users", s.requireOperationAuth(s.createUser))
	routes.action("/api/v1/nodes/{node_id}/users/{user_action}", "user_action", nil, s.requireOperationAuth(s.userAction), "disable", "enable", "rotate-password")
	routes.HandleFunc("PUT /api/v1/nodes/{node_id}/groups/{group_name}", s.requireOperationAuth(s.applyGroup))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/users/{username}/policy", s.requireOperationAuth(s.getUserPolicy))
	routes.HandleFunc("PUT /api/v1/nodes/{node_id}/users/{username}/policy", s.requireOperationAuth(s.setUserPolicy))
}

func (s *Server) registerConfigPlanRoutes(routes *methodRegistrar) {
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/config-plans", s.requireOperationAuth(s.createConfigPlan))
	routes.HandleFunc("GET /api/v1/config-plans/{plan_id}", s.requireOperationAuth(s.getConfigPlan))
	routes.HandleFunc("POST /api/v1/config-plans/{plan_id}/apply", s.requireOperationAuth(s.applyConfigPlan))
}

func (s *Server) registerCertificateRoutes(routes *methodRegistrar) {
	routes.HandleFunc("POST /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.createCertificate))
	routes.HandleFunc("GET /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.listNodeCertificates))
	routes.action("/api/v1/certificates/{certificate_id}", "certificate_id", s.requireOperationAuth(s.getCertificate), s.requireOperationAuth(s.certificateAction), "issue", "revoke", "p12")
	routes.HandleFunc("GET /api/v1/artifacts/{artifact_id}", s.requireOperationAuth(s.downloadArtifact))
	routes.HandleFunc("POST /api/v1/secret-provider-refs", s.requireOperationAuth(s.createSecretRef))
	routes.action("/api/v1/secret-provider-refs/{secret_ref_id}", "secret_ref_id", s.requireOperationAuth(s.getSecretRef), s.requireOperationAuth(s.rotateSecretRef), "rotate")
}

func (s *Server) registerAuthorizationRoutes(routes *methodRegistrar) {
	routes.HandleFunc("GET /api/v1/audit/vpn", s.requireOperationAuth(s.listVpnAuditRecords))
	routes.HandleFunc("GET /api/v1/audit/events", s.requireOperationAuth(s.listAuditEvents))
	routes.HandleFunc("POST /api/v1/audit:verify", s.requireOperationAuth(s.verifyAudit))
	routes.HandleFunc("GET /api/v1/workspaces", s.requireOperationAuth(s.listWorkspaces))
}
