package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/audit"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func TestHTTPAssemblyFailureOwnership(t *testing.T) {
	for _, failure := range []string{"SSE", "authentication", "none"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			life := newLifecycle(ctx, time.Second, quietLogger())
			started, stopped := make(chan struct{}), make(chan struct{})
			life.start("already started", func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				close(stopped)
				return ctx.Err()
			})
			<-started
			cfg := config.Config{EventStreams: eventstream.DefaultConfig(), RequestTimeout: time.Second}
			if failure == "SSE" {
				cfg.EventStreams.Watchers = 0
			}
			if failure == "authentication" {
				cfg.LocalAuth = true
			}
			server, err := newHTTPServer(life, cfg, BuildInfo{}, stoppedBackend{}, nil, quietLogger(), httpServices{})
			if failure == "none" {
				if err != nil || server == nil || life.http != server {
					t.Fatalf("ownership not transferred before listen: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "configure "+failure) || server != nil || life.http != nil {
					t.Fatalf("partial HTTP construction: %v %v", server, err)
				}
			}
			cancel()
			result := life.close(err)
			if failure != "none" && (!errors.Is(result, err) || errors.Is(result, context.Canceled)) {
				t.Fatalf("primary error lost to cancellation: %v", result)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("construction exit left a running task")
			}
		})
	}
}

func TestNonAPIRolesSkipHTTPConstruction(t *testing.T) {
	for _, role := range []config.Role{config.RoleWorker, config.RoleScheduler} {
		t.Run(string(role), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			// Both SSE and authentication would fail if the HTTP path ran.
			cfg := config.Config{Role: role, LocalAuth: true, ShutdownTimeout: time.Second, UserOperationConcurrency: 1, AgentUpgradeReconcile: time.Minute}
			backend := stoppedBackend{}
			err := runRoles(ctx, cfg, BuildInfo{}, backend, audit.NewBackendManager(backend, nil), quietLogger())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("non-API role constructed HTTP/authentication: %v", err)
			}
		})
	}
}
