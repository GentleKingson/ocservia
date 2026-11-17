package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/browserorigin"
	"github.com/GentleKingson/ocservia/control-plane/internal/certificates"
	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportevents"
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
	http                *http.Server
	backend             database.Backend
	build               BuildInfo
	logger              *slog.Logger
	bodyLimit           int64
	requestTimeout      time.Duration
	devAuth             bool
	devAuthToken        string
	browserOrigin       string
	transportEvents     *transportevents.Service
	localSimulator      bool
	operations          *operations.Service
	enrollment          *enrollment.Service
	transport           *transportclient.Client
	fences              ownersession.FencedExecutor
	nodes               NodeReader
	registeredMethods   []routeMethodRule
	releaseCatalog      *releasecatalog.Catalog
	auth                *auth.Service
	authProxies         []netip.Prefix
	breakGlassBudget    *authAdmission
	localLoginBudget    *authAdmission
	privdRegisterBudget *authAdmission
	authLogs            authLogState
	audit               *audit.Manager
	userstate           *userstate.Service
	userOperations      UserOperations
	configPlans         ConfigPlans
	certificates        *certificates.Service
	eventStreamsMu      sync.Mutex
	eventConfig         eventstream.Config
	eventAdmission      *eventstream.Manager
	platformEvents      *eventstream.Hub
	operationEvents     *eventstream.Hub
	requestsMu          sync.Mutex
	requests            sync.WaitGroup
	stopping            bool
}

// HTTPConfig contains final HTTP values. EventStreams must be explicitly valid;
// callers wanting defaults should supply eventstream.DefaultConfig().
type HTTPConfig struct {
	Address            string
	BodyLimit          int64
	RequestTimeout     time.Duration
	DevAuth            bool
	DevAuthToken       string
	LocalSimulator     bool
	BrowserOrigin      string
	AuthTrustedProxies []netip.Prefix
	EventStreams       eventstream.Config
}

// Modules is construction input only. Nil capabilities disable the optional feature, not its routes.
type Modules struct {
	Nodes           NodeReader
	ConfigPlans     ConfigPlans
	UserOperations  UserOperations
	Certificates    *certificates.Service
	Operations      *operations.Service
	ReleaseCatalog  *releasecatalog.Catalog
	UserState       *userstate.Service
	Enrollment      *enrollment.Service
	Transport       *transportclient.Client
	OwnerFencing    ownersession.FencedExecutor
	TransportEvents *transportevents.Service
}

type Authorization struct {
	Authentication *auth.Service
	Audit          *audit.Manager
}

// NewServer completes module and SSE construction before registering routes.
// It owns partial HTTP resources until success; the caller then owns Shutdown.
func NewServer(config HTTPConfig, backend database.Backend, build BuildInfo, logger *slog.Logger, modules Modules, authorization Authorization) (*Server, error) {
	if err := config.EventStreams.Validate(); err != nil {
		return nil, fmt.Errorf("configure SSE admission: %w", err)
	}
	if service, ok := modules.Nodes.(*telemetry.Service); ok && service == nil {
		modules.Nodes = nil
	}
	if service, ok := modules.ConfigPlans.(*configplan.Service); ok && service == nil {
		modules.ConfigPlans = nil
	}
	if service, ok := modules.UserOperations.(*useroperations.Service); ok && service == nil {
		modules.UserOperations = nil
	}
	s := &Server{
		backend: backend, build: build, logger: logger, bodyLimit: config.BodyLimit,
		requestTimeout: config.RequestTimeout, devAuth: config.DevAuth, devAuthToken: config.DevAuthToken,
		auth: authorization.Authentication, audit: authorization.Audit,
		nodes: modules.Nodes, configPlans: modules.ConfigPlans,
		userOperations: modules.UserOperations, certificates: modules.Certificates,
		operations: modules.Operations, releaseCatalog: modules.ReleaseCatalog, userstate: modules.UserState,
		enrollment: modules.Enrollment, transport: modules.Transport, fences: modules.OwnerFencing,
		transportEvents: modules.TransportEvents, localSimulator: config.LocalSimulator,
		authProxies: append([]netip.Prefix(nil), config.AuthTrustedProxies...),
	}
	if config.BrowserOrigin != "" {
		normalized, ok := browserorigin.Normalize(config.BrowserOrigin)
		if ok {
			s.browserOrigin = normalized
		} else {
			logger.Warn("ignoring unparseable browser origin", "origin_length", len(config.BrowserOrigin))
		}
	}
	s.breakGlassBudget = newAuthAdmission(5, 0, 4)
	s.localLoginBudget = newAuthAdmission(5, 120, 4)
	s.privdRegisterBudget = newAuthAdmission(30, 120, 4)
	if err := s.initEventStreams(config.EventStreams); err != nil {
		return nil, fmt.Errorf("configure SSE admission: %w", err)
	}
	mux := http.NewServeMux()
	s.registeredMethods = s.registerRoutes(mux)
	handler := s.requestContext(s.limitBody(s.timeout(s.routeErrors(mux))))
	// Bound request reads without a global write deadline that would end SSE streams.
	s.http = &http.Server{Addr: config.Address, Handler: otelhttp.NewHandler(handler, "http.server"), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	return s, nil
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
		err = errors.Join(err, s.http.Close())
	}
	done := make(chan struct{})
	go func() { s.requests.Wait(); close(done) }()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}
