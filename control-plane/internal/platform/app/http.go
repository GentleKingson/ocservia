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
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
)

func newHTTPServer(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, expectedSchemaVersion int64, logger *slog.Logger) (*api.Server, error) {
	var err error
	server := api.NewBackend(cfg.HTTPAddress, backend, api.BuildInfo{Version: build.Version, Commit: build.Commit, Role: string(cfg.Role), RecommendedAgentVersion: cfg.RecommendedAgentVersion}, logger, cfg.BodyLimit, cfg.RequestTimeout, operationAuthEnabled(cfg), cfg.DevAuthToken, expectedSchemaVersion)
	life.http = server
	server.EnableBrowserOrigin(cfg.BrowserOrigin())
	server.ConfigureAuthProxies(cfg.AuthTrustedProxyCIDRs)
	if err := server.ConfigureEventStreams(cfg.EventStreams); err != nil {
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
	server.EnableAuthorization(authService, rbac.NewBackend(backend), approvals.NewBackend(backend), auditManager)

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
