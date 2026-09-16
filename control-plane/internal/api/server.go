package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api/nodehttp"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/browserorigin"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type BuildInfo struct {
	Version                 string `json:"version"`
	Commit                  string `json:"commit"`
	Role                    string `json:"role"`
	RecommendedAgentVersion string `json:"recommended_agent_version,omitempty"`
}

type Server struct {
	http             *http.Server
	backend          database.Backend
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
	nodeHTTP         *nodehttp.Handler
	releaseCatalog   *releasecatalog.Catalog
	auth             *auth.Service
	authProxies      []netip.Prefix
	breakGlassBudget *authAdmission
	localLoginBudget *authAdmission
	authLogs         authLogState
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
	requestsMu       sync.Mutex
	requests         sync.WaitGroup
	stopping         bool
}

func NewBackend(address string, backend database.Backend, build BuildInfo, logger *slog.Logger, bodyLimit int64, requestTimeout time.Duration, devAuth bool, devAuthToken string, expectedSchema int64) *Server {
	s := &Server{backend: backend, build: build, logger: logger, bodyLimit: bodyLimit, requestTimeout: requestTimeout, devAuth: devAuth, devAuthToken: devAuthToken, expectedSchema: expectedSchema}
	s.breakGlassBudget = newAuthAdmission(5, 0, 4)
	s.localLoginBudget = newAuthAdmission(5, 120, 4)
	if err := s.configureEventStreams(eventstream.DefaultConfig()); err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	s.nodeHTTP = nodehttp.New(logger, workspace)
	s.registerRoutes(mux)
	handler := s.requestContext(s.limitBody(s.timeout(s.routeErrors(mux))))
	// Bound request reads without a global write deadline that would end SSE streams.
	s.http = &http.Server{Addr: address, Handler: otelhttp.NewHandler(handler, "http.server"), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}

var _ nodehttp.Reader = (*telemetrystore.Service)(nil)

// EnableTelemetry supplies the configured read service before HTTP starts.
func (s *Server) EnableTelemetry(service *telemetrystore.Service) {
	if service == nil {
		s.nodeHTTP.SetReader(nil)
		return
	}
	s.nodeHTTP.SetReader(service)
}

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
	s.requestsMu.Lock()
	s.stopping = true
	s.requestsMu.Unlock()
	s.closeEventStreams()
	err := s.http.Shutdown(ctx)
	if err != nil {
		// Shutdown alone leaves active connections open after its deadline.
		err = errors.Join(err, s.http.Close())
	}
	// TimeoutHandler can finish the HTTP request before its inner database
	// handler returns. Admission is closed above, so no Add can race this wait.
	done := make(chan struct{})
	go func() { s.requests.Wait(); close(done) }()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
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
