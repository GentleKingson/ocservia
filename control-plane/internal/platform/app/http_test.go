package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/api"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func TestHTTPAssemblyFailureOwnership(t *testing.T) {
	for _, failure := range []string{"SSE", "authentication", "secure node installation", "none"} {
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
			modules := api.Modules{}
			if failure == "secure node installation" {
				cfg.InstallationRelayURL = "http://invalid-relay.example"
				modules.Enrollment = enrollment.NewBackend(stoppedBackend{}, "", "test", nil)
			}
			server, err := newHTTPServer(life, cfg, BuildInfo{}, stoppedBackend{}, nil, quietLogger(), modules)
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
