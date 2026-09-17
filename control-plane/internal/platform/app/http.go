package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers handlers on the internal pprof listener only
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/approvals"
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/localslice"
	"github.com/GentleKingson/ocservia/control-plane/internal/operations"
	"github.com/GentleKingson/ocservia/control-plane/internal/ownersession"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/privdattestation"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/releasecatalog"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"github.com/GentleKingson/ocservia/control-plane/internal/userstate"
)

// HTTP consumers reuse services already configured by runRoles. This input is
// not retained by Server or passed to any handler.
type httpServices struct {
	modules        api.Modules
	operations     *operations.Service
	releaseCatalog *releasecatalog.Catalog
	userState      *userstate.Service
	enrollment     *enrollment.Service
	transport      *transportclient.Client
	fences         ownersession.FencedExecutor
	localSlice     *localslice.Service
}

func newHTTPServer(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, expectedSchemaVersion int64, logger *slog.Logger, services httpServices) (*api.Server, error) {
	var err error
	if err := cfg.EventStreams.Validate(); err != nil {
		return nil, fmt.Errorf("configure SSE admission: %w", err)
	}
	var authService *auth.Service
	if cfg.LocalAuthEnabled() || cfg.OIDCEnabled() {
		authConfig := auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL, BreakGlassEnabled: cfg.BreakGlassEnabled, BreakGlassTokenHash: cfg.BreakGlassTokenHash}
		if cfg.OIDCEnabled() {
			authConfig.Issuer, authConfig.ClientID = cfg.OIDCIssuer, cfg.OIDCClientID
			authConfig.ClientSecret, authConfig.RedirectURL = cfg.OIDCClientSecret, cfg.OIDCRedirectURL
		}
		authService, err = auth.NewBackend(backend, authConfig)
		if err != nil {
			return nil, fmt.Errorf("configure authentication: %w", err)
		}
	}
	server, err := api.NewServer(api.HTTPConfig{
		Address: cfg.HTTPAddress, BodyLimit: cfg.BodyLimit, RequestTimeout: cfg.RequestTimeout,
		DevAuth: operationAuthEnabled(cfg), DevAuthToken: cfg.DevAuthToken, ExpectedSchema: expectedSchemaVersion,
		BrowserOrigin: cfg.BrowserOrigin(), AuthTrustedProxies: cfg.AuthTrustedProxyCIDRs, EventStreams: cfg.EventStreams,
	}, backend, api.BuildInfo{Version: build.Version, Commit: build.Commit, Role: string(cfg.Role), RecommendedAgentVersion: cfg.RecommendedAgentVersion}, logger, services.modules,
		api.Authorization{Authentication: authService, RBAC: rbac.NewBackend(backend), Approvals: approvals.NewBackend(backend), Audit: auditManager})
	if err != nil {
		return nil, err
	}
	// No HTTP resources exist on earlier failures. From this point lifecycle
	// owns Shutdown, including failures before the listener starts.
	life.http = server
	server.EnableOperations(services.operations)
	server.EnableReleaseCatalog(services.releaseCatalog)
	server.EnableUserState(services.userState)
	server.EnablePrivdAttestation(privdattestation.NewBackend(backend))
	server.EnableEnrollment(services.enrollment, services.transport)
	server.EnableOwnerFencing(services.fences)
	server.EnableLocalSlice(services.localSlice)
	server.SetLocalSimulatorEnabled(cfg.LocalSimulator)

	return server, nil
}

func startPprof(address string, logger *slog.Logger) func() {
	if address == "" {
		return func() {}
	}
	server := &http.Server{Addr: address, Handler: http.DefaultServeMux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof listener failed", "error", err)
		}
	}()
	logger.Info("loopback pprof listener serving", "address", address)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("pprof shutdown failed", "error", err)
			if err := server.Close(); err != nil {
				logger.Error("pprof close failed", "error", err)
			}
		}
		select {
		case <-done:
		case <-ctx.Done():
			logger.Error("pprof listener did not stop", "error", ctx.Err())
		}
	}
}

func operationAuthEnabled(cfg config.Config) bool { return cfg.DevAuth }
