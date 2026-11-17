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
	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func newHTTPServer(life *lifecycle, cfg config.Config, build BuildInfo, backend database.Backend, auditManager *audit.Manager, logger *slog.Logger, modules api.Modules) (*api.Server, error) {
	var err error
	if err := cfg.EventStreams.Validate(); err != nil {
		return nil, fmt.Errorf("configure SSE admission: %w", err)
	}
	var authService *auth.Service
	if cfg.LocalAuthEnabled() || cfg.BreakGlassEnabled || len(cfg.SessionKey) == 32 {
		authConfig := auth.Config{LocalEnabled: cfg.LocalAuthEnabled(), SessionKey: cfg.SessionKey, SessionTTL: cfg.SessionTTL, BreakGlassEnabled: cfg.BreakGlassEnabled, BreakGlassTokenHash: cfg.BreakGlassTokenHash}
		authService, err = auth.NewBackend(backend, authConfig)
		if err != nil {
			return nil, fmt.Errorf("configure authentication: %w", err)
		}
	}
	if modules.Enrollment != nil && (cfg.InstallationRelayURL != "" || cfg.InstallationRelayToken != "") {
		if err := modules.Enrollment.ConfigureInstallation(enrollment.InstallationTrust{Origin: cfg.BrowserOrigin(), RelayURL: cfg.InstallationRelayURL, RelayToken: cfg.InstallationRelayToken}); err != nil {
			return nil, fmt.Errorf("configure secure node installation: %w", err)
		}
	}
	server, err := api.NewServer(api.HTTPConfig{
		Address: cfg.HTTPAddress, BodyLimit: cfg.BodyLimit, RequestTimeout: cfg.RequestTimeout,
		DevAuth: operationAuthEnabled(cfg), DevAuthToken: cfg.DevAuthToken,
		LocalSimulator: cfg.LocalSimulator, BrowserOrigin: cfg.BrowserOrigin(), AuthTrustedProxies: cfg.AuthTrustedProxyCIDRs, EventStreams: cfg.EventStreams,
	}, backend, api.BuildInfo{Version: build.Version, Commit: build.Commit, RecommendedAgentVersion: cfg.RecommendedAgentVersion}, logger, modules,
		api.Authorization{Authentication: authService, Audit: auditManager})
	if err != nil {
		return nil, err
	}
	// No HTTP resources exist on earlier failures. From this point lifecycle
	// owns Shutdown, including failures before the listener starts.
	life.http = server
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
