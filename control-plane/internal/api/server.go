package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/browserorigin"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	telemetrystore "github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
	"github.com/GentleKingson/ocservia/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

type BuildInfo struct {
	Version                 string `json:"version"`
	Commit                  string `json:"commit"`
	Role                    string `json:"role"`
	RecommendedAgentVersion string `json:"recommended_agent_version,omitempty"`
}

type Server struct {
	http             *http.Server
	pool             *pgxpool.Pool
	build            BuildInfo
	logger           *slog.Logger
	bodyLimit        int64
	requestTimeout   time.Duration
	devAuth          bool
	devAuthToken     string
	browserOrigin    string
	expectedSchema   int64
	localSlice       *localslice.Service
	localSliceMu     sync.RWMutex
	localSimulator   bool
	operations       *operationstore.Service
	enrollment       *enrollment.Service
	transport        *transportclient.Client
	fences           ownersession.FencedExecutor
	telemetry        *telemetrystore.Service
	releaseCatalog   *releasecatalog.Catalog
	auth             *auth.Service
	rbac             *rbac.Service
	approvals        *approvals.Service
	audit            *audit.Manager
	userstate        *userstate.Service
	useroperations   *useroperations.Service
	configplans      *configplan.Service
	certificates     *certificates.Service
	privdAttestation *privdattestation.Service
	eventStreamsMu   sync.Mutex
	eventConfig      eventstream.Config
	eventAdmission   *eventstream.Manager
	platformEvents   *eventstream.Hub
	operationEvents  *eventstream.Hub
}

func New(address string, pool *pgxpool.Pool, build BuildInfo, logger *slog.Logger, bodyLimit int64, requestTimeout time.Duration, devAuth bool, devAuthToken string, expectedSchema int64) *Server {
	s := &Server{pool: pool, build: build, logger: logger, bodyLimit: bodyLimit, requestTimeout: requestTimeout, devAuth: devAuth, devAuthToken: devAuthToken, expectedSchema: expectedSchema}
	if err := s.configureEventStreams(eventstream.DefaultConfig()); err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", s.live)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /api/v1/livez", s.live)
	mux.HandleFunc("GET /api/v1/readyz", s.ready)
	mux.HandleFunc("GET /api/v1/version", s.version)
	mux.HandleFunc("GET /api/v1/auth/login", s.login)
	mux.HandleFunc("GET /api/v1/auth/callback", s.callback)
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireOperationAuth(s.logout))
	mux.HandleFunc("POST /api/v1/auth/break-glass", s.breakGlass)
	mux.HandleFunc("POST /api/v1/development/simulations", s.createSimulation)
	mux.HandleFunc("GET /api/v1/development/runtime", s.developmentRuntime)
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
	mux.HandleFunc("POST /api/v1/enrollment-tokens", s.requireOperationAuth(s.createEnrollmentToken))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/approval", s.requireOperationAuth(s.approveNode))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/revocation", s.requireOperationAuth(s.revokeNode))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-credentials", s.requireOperationAuth(s.createPrivdAttestationCredential))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-keys:register", s.registerPrivdAttestationKey)
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/privd-attestation-keys:revoke", s.requireOperationAuth(s.revokePrivdAttestationKey))
	mux.HandleFunc("GET /api/v1/nodes", s.requireOperationAuth(s.listNodes))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}", s.requireOperationAuth(s.getNode))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/sessions", s.requireOperationAuth(s.listNodeSessions))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/ip-bans", s.requireOperationAuth(s.listNodeIPBans))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/telemetry", s.requireOperationAuth(s.listNodeTelemetry))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/user-group-state", s.requireOperationAuth(s.listUserGroupState))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/users", s.requireOperationAuth(s.createUser))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/users/{user_action}", s.requireOperationAuth(s.userAction))
	mux.HandleFunc("PUT /api/v1/nodes/{node_id}/groups/{group_name}", s.requireOperationAuth(s.applyGroup))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/users/{username}/policy", s.requireOperationAuth(s.getUserPolicy))
	mux.HandleFunc("PUT /api/v1/nodes/{node_id}/users/{username}/policy", s.requireOperationAuth(s.setUserPolicy))
	mux.HandleFunc("POST /api/v1/user-batches", s.requireOperationAuth(s.createUserBatch))
	mux.HandleFunc("GET /api/v1/user-batches/{batch_id}", s.requireOperationAuth(s.getUserBatch))
	mux.HandleFunc("POST /api/v1/agent-rollouts", s.requireOperationAuth(s.createAgentRollout))
	mux.HandleFunc("GET /api/v1/agent-rollouts", s.requireOperationAuth(s.listAgentRollouts))
	mux.HandleFunc("GET /api/v1/agent-rollouts/{rollout_id}", s.requireOperationAuth(s.getAgentRollout))
	mux.HandleFunc("POST /api/v1/agent-rollouts/{rollout_id}/resume", s.requireOperationAuth(s.resumeAgentRollout))
	mux.HandleFunc("GET /api/v1/user-operations/metrics", s.requireOperationAuth(s.userOperationMetrics))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/config-plans", s.requireOperationAuth(s.createConfigPlan))
	mux.HandleFunc("GET /api/v1/config-plans/{plan_id}", s.requireOperationAuth(s.getConfigPlan))
	mux.HandleFunc("POST /api/v1/config-plans/{plan_id}/apply", s.requireOperationAuth(s.applyConfigPlan))
	mux.HandleFunc("POST /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.createCertificate))
	mux.HandleFunc("GET /api/v1/nodes/{node_id}/certificates", s.requireOperationAuth(s.listNodeCertificates))
	mux.HandleFunc("GET /api/v1/certificates/{certificate_id}", s.requireOperationAuth(s.getCertificate))
	mux.HandleFunc("POST /api/v1/certificates/{certificate_action}", s.requireOperationAuth(s.certificateAction))
	mux.HandleFunc("GET /api/v1/artifacts/{artifact_id}", s.requireOperationAuth(s.downloadArtifact))
	mux.HandleFunc("POST /api/v1/secret-provider-refs", s.requireOperationAuth(s.createSecretRef))
	mux.HandleFunc("GET /api/v1/secret-provider-refs/{secret_ref_id}", s.requireOperationAuth(s.getSecretRef))
	mux.HandleFunc("POST /api/v1/secret-provider-refs/{secret_ref_action}", s.requireOperationAuth(s.rotateSecretRef))
	mux.HandleFunc("POST /api/v1/approval-requests", s.requireOperationAuth(s.createApproval))
	mux.HandleFunc("GET /api/v1/approval-requests/{approval_id}", s.requireOperationAuth(s.getApproval))
	mux.HandleFunc("POST /api/v1/approval-requests/{approval_id}", s.requireOperationAuth(s.approveRequest))
	mux.HandleFunc("GET /api/v1/audit/events", s.requireOperationAuth(s.listAuditEvents))
	mux.HandleFunc("POST /api/v1/audit:verify", s.requireOperationAuth(s.verifyAudit))
	mux.HandleFunc("GET /api/v1/workspaces", s.requireOperationAuth(s.listWorkspaces))
	mux.HandleFunc("POST /api/v1/role-bindings", s.requireOperationAuth(s.createRoleBinding))
	handler := s.requestContext(s.limitBody(s.timeout(s.routeErrors(mux))))
	s.http = &http.Server{Addr: address, Handler: otelhttp.NewHandler(handler, "http.server"), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}

func (s *Server) EnableTelemetry(service *telemetrystore.Service) { s.telemetry = service }

// EnableBrowserOrigin installs the exact public browser origin that may spend
// a session cookie on state changing requests. An origin that does not
// normalize is rejected so a misconfigured deployment fails closed instead of
// trusting malformed input.
func (s *Server) EnableBrowserOrigin(origin string) {
	if origin == "" {
		return
	}
	normalized, ok := browserorigin.Normalize(origin)
	if !ok {
		s.logger.Warn("ignoring unparseable browser origin", "origin_length", len(origin))
		return
	}
	s.browserOrigin = normalized
}

func (s *Server) EnableOperations(service *operationstore.Service) { s.operations = service }

// EnableReleaseCatalog installs the operator-provisioned trusted agent
// release catalog backing the single-node upgrade workflow.
func (s *Server) EnableReleaseCatalog(catalog *releasecatalog.Catalog) { s.releaseCatalog = catalog }

func (s *Server) EnableUserState(service *userstate.Service) { s.userstate = service }

func (s *Server) EnableUserOperations(service *useroperations.Service) { s.useroperations = service }

func (s *Server) EnableConfigPlans(service *configplan.Service) { s.configplans = service }

func (s *Server) EnableCertificates(service *certificates.Service) { s.certificates = service }

func (s *Server) EnablePrivdAttestation(service *privdattestation.Service) {
	s.privdAttestation = service
}

func (s *Server) certificateAction(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.PathValue("certificate_action"), ":issue"):
		s.issueCertificate(w, r)
	case strings.HasSuffix(r.PathValue("certificate_action"), ":revoke"):
		s.revokeCertificate(w, r)
	case strings.HasSuffix(r.PathValue("certificate_action"), ":p12"):
		s.createCertificateP12(w, r)
	default:
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the certificate action does not exist")
	}
}

func (s *Server) EnableAuthorization(authn *auth.Service, authz *rbac.Service, approvalService *approvals.Service, auditManager *audit.Manager) {
	s.auth, s.rbac, s.approvals, s.audit = authn, authz, approvalService, auditManager
}

func (s *Server) EnableEnrollment(service *enrollment.Service, transport *transportclient.Client) {
	s.enrollment = service
	s.transport = transport
}

// EnableOwnerFencing runs administrative trust updates and connection closes
// issued by API handlers inside the connection owner's fencing interval, so
// a stale owner cannot drive connection state through the API role.
func (s *Server) EnableOwnerFencing(fences ownersession.FencedExecutor) {
	s.fences = fences
}

func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.closeEventStreams()
	return s.http.Shutdown(ctx)
}

func (s *Server) EnableLocalSlice(service *localslice.Service) {
	s.localSliceMu.Lock()
	defer s.localSliceMu.Unlock()
	s.localSlice = service
	s.localSimulator = true
}

func (s *Server) SetLocalSimulatorEnabled(enabled bool) { s.localSimulator = enabled }

func (s *Server) localSliceService() *localslice.Service {
	s.localSliceMu.RLock()
	defer s.localSliceMu.RUnlock()
	return s.localSlice
}

func (s *Server) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/database-unavailable", "Service is not ready", "database dependency is unavailable")
		return
	}
	version, err := migrations.CurrentSchemaVersion(ctx, s.pool)
	if err != nil || version != s.expectedSchema {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/schema-unavailable", "Service is not ready", "database schema is unavailable")
		return
	}
	_, platformHub, operationHub := s.eventStreamSnapshots()
	if platformHub.UnhealthyWatchers+operationHub.UnhealthyWatchers > 0 {
		writeProblem(w, r, http.StatusServiceUnavailable, "https://ocservia.dev/problems/event-stream-unavailable", "Service is not ready", "event stream watcher is recovering")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "schema_version": version})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.build)
}

func (s *Server) developmentRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.localSimulator {
		writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
		return
	}
	pool := s.pool.Stat()
	admission, platformHub, operationHub := s.eventStreamSnapshots()
	metricsContext, cancelMetrics := context.WithTimeout(r.Context(), time.Second)
	defer cancelMetrics()
	keyStates, err := privdattestation.KeyStateMetrics(metricsContext, s.pool)
	keyStatesAvailable := err == nil
	if err != nil {
		// Keep process and SSE diagnostics available while PostgreSQL is down.
		// The availability bit prevents the bounded zero-value series from being
		// mistaken for a successful database observation.
		keyStates, _ = privdattestation.KeyStateMetrics(r.Context(), nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"goroutines":                             runtime.NumGoroutine(),
		"db_acquired":                            pool.AcquiredConns(),
		"db_idle":                                pool.IdleConns(),
		"db_total":                               pool.TotalConns(),
		"sse_active_streams":                     admission.Active,
		"sse_rejected_streams":                   admission.RejectedGlobal + admission.RejectedIdentity + admission.RejectedSession + admission.RejectedWorkspace + admission.RejectedResource,
		"sse_watchers":                           platformHub.Watchers + operationHub.Watchers,
		"sse_unhealthy_watchers":                 platformHub.UnhealthyWatchers + operationHub.UnhealthyWatchers,
		"sse_sql_queries":                        platformHub.Queries + operationHub.Queries,
		"sse_slow_consumer_disconnects":          platformHub.SlowConsumerDisconnects + operationHub.SlowConsumerDisconnects,
		"sse_database_backoff_seconds":           (platformHub.DatabaseBackoff + operationHub.DatabaseBackoff).Seconds(),
		"privd_receipt_verifications":            privdattestation.VerificationMetrics(),
		"privd_attestation_key_states":           keyStates,
		"privd_attestation_key_states_available": keyStatesAvailable,
	})
}

func (s *Server) routeErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedMethod, ok := routeMethod(r.URL.Path)
		if !ok {
			writeProblem(w, r, http.StatusNotFound, "https://ocservia.dev/problems/not-found", "Resource not found", "the requested resource does not exist")
			return
		}
		methodAllowed := r.Method == expectedMethod || expectedMethod == "GET_OR_PUT" && (r.Method == http.MethodGet || r.Method == http.MethodPut) || expectedMethod == "GET_OR_POST" && (r.Method == http.MethodGet || r.Method == http.MethodPost)
		if !methodAllowed {
			allow := expectedMethod
			if expectedMethod == "GET_OR_PUT" {
				allow = "GET, PUT"
			} else if expectedMethod == "GET_OR_POST" {
				allow = "GET, POST"
			}
			w.Header().Set("Allow", allow)
			writeProblem(w, r, http.StatusMethodNotAllowed, "https://ocservia.dev/problems/method-not-allowed", "Method not allowed", "the requested method is not supported")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func routeMethod(path string) (string, bool) {
	switch path {
	case "/livez", "/readyz", "/version", "/api/v1/livez", "/api/v1/readyz", "/api/v1/version", "/api/v1/operations", "/api/v1/operations/queue-metrics", "/api/v1/operations/summary", "/api/v1/user-operations/metrics", "/api/v1/events", "/api/v1/events/stream", "/api/v1/development/runtime", "/api/v1/auth/login", "/api/v1/auth/callback", "/api/v1/audit/events", "/api/v1/workspaces":
		return http.MethodGet, true
	case "/api/v1/nodes":
		return http.MethodGet, true
	case "/api/v1/development/simulations":
		return http.MethodPost, true
	case "/api/v1/enrollment-tokens", "/api/v1/auth/logout", "/api/v1/auth/break-glass", "/api/v1/approval-requests", "/api/v1/audit:verify", "/api/v1/role-bindings", "/api/v1/user-batches":
		return http.MethodPost, true
	}
	if path == "/api/v1/secret-provider-refs" {
		return http.MethodPost, true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "certificates" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":issue") || strings.HasSuffix(parts[3], ":revoke") || strings.HasSuffix(parts[3], ":p12") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "artifacts" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "secret-provider-refs" && parts[3] != "" {
		if strings.HasSuffix(parts[3], ":rotate") {
			return http.MethodPost, true
		}
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "approval" || parts[4] == "revocation") {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "privd-attestation-credentials" || parts[4] == "privd-attestation-keys:register" || parts[4] == "privd-attestation-keys:revoke") {
		return http.MethodPost, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && (parts[4] == "sessions" || parts[4] == "telemetry" || parts[4] == "ip-bans" || parts[4] == "user-group-state") {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" && (strings.HasSuffix(parts[5], ":disable") || strings.HasSuffix(parts[5], ":enable") || strings.HasSuffix(parts[5], ":rotate-password")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "groups" && parts[5] != "" {
		return http.MethodPut, true
	}
	if len(parts) == 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "users" && parts[5] != "" && parts[6] == "policy" {
		return "GET_OR_PUT", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "user-batches" && parts[3] != "" {
		return http.MethodGet, true
	}
	if path == "/api/v1/agent-rollouts" {
		return "GET_OR_POST", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "agent-rollouts" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "agent-rollouts" && parts[3] != "" && parts[4] == "resume" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "synthetic-commands" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "config-plans" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "certificates" {
		return "GET_OR_POST", true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "config-plans" && parts[3] != "" {
		return http.MethodGet, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "sessions" && (strings.HasSuffix(parts[5], ":disconnect") || strings.HasSuffix(parts[5], ":terminate")) {
		return http.MethodPost, true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "ip-bans" && strings.HasSuffix(parts[5], ":remove") {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "service:reload" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "nodes" && parts[3] != "" && parts[4] == "agent-upgrade" {
		return http.MethodPost, true
	}
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "operations" && parts[3] != "" && parts[4] == "events" {
		return http.MethodGet, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "approval-requests" && strings.HasSuffix(parts[3], ":approve") {
		return http.MethodPost, true
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "approval-requests" && parts[3] != "" {
		return http.MethodGet, true
	}
	operationID := strings.TrimPrefix(path, "/api/v1/operations/")
	if operationID != path && operationID != "" && !strings.Contains(operationID, "/") {
		return http.MethodGet, true
	}
	return "", false
}

func (s *Server) timeout(next http.Handler) http.Handler {
	timed := http.TimeoutHandler(next, s.requestTimeout, `{"type":"https://ocservia.dev/problems/timeout","title":"Request timed out","status":503}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events/stream" || strings.HasSuffix(r.URL.Path, "/events") && strings.HasPrefix(r.URL.Path, "/api/v1/operations/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		timed.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = randomID()
		}
		w.Header().Set("X-Request-ID", requestID)
		if s.hasOperationPrincipal(r) {
			w.Header().Set("X-Ocservia-Dev-Subject", "developer")
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		s.logger.InfoContext(r.Context(), "http request", "request_id", requestID, "trace_id", trace.SpanContextFromContext(r.Context()).TraceID().String(), "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireOperationAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "OIDC")
			writeProblem(w, r, http.StatusUnauthorized, "https://ocservia.dev/problems/unauthenticated", "Authentication required", "operation state requires an authenticated principal")
			return
		}
		if err := s.validateBrowserMutation(r, principal); err != nil {
			writeProblem(w, r, http.StatusForbidden, "https://ocservia.dev/problems/cross-origin-request", "Cross-origin request", err.Error())
			return
		}
		ctx, err := s.authorizeRoute(r, principal)
		if err != nil {
			s.writeAuthorizationError(w, r, err)
			return
		}
		next(w, r.WithContext(ctx))
	}
}

var errCrossOrigin = errors.New("the request Origin does not match the trusted browser origin")

// validateBrowserMutation enforces the browser trust boundary for state
// changing requests: a session cookie established through OIDC or break-glass
// may only be spent by the exact public browser origin, so a sibling origin
// on the same site, an unknown site, or a missing Origin cannot drive a
// mutation. Development bearer principals are non-browser credentials and
// safe methods never mutate state, so neither requires an Origin. A
// cross-site Fetch Metadata signal is rejected even before the Origin
// comparison, while same-site and same-origin signals never replace it.
func (s *Server) validateBrowserMutation(r *http.Request, principal auth.Principal) error {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil
	}
	if principal.Issuer == "development" {
		return nil
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return errCrossOrigin
	}
	if s.browserOrigin == "" {
		return errCrossOrigin
	}
	origin, ok := browserorigin.Normalize(r.Header.Get("Origin"))
	if !ok || origin != s.browserOrigin {
		return errCrossOrigin
	}
	return nil
}

func (s *Server) hasOperationPrincipal(r *http.Request) bool {
	if s.devAuth {
		return true
	}
	const prefix = "Bearer "
	authorization := r.Header.Get("Authorization")
	if s.devAuthToken == "" || !strings.HasPrefix(authorization, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))), []byte(s.devAuthToken)) == 1
}

type requestIDKey struct{}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, kind, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "title": title, "status": status, "detail": detail, "instance": r.URL.Path, "trace_id": trace.SpanContextFromContext(r.Context()).TraceID().String()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(data[:])
}
