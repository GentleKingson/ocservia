package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/connection"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

type roleTransport struct {
	transportv1.UnimplementedTransportServiceServer
	active atomic.Int32
}

func (s *roleTransport) WatchEvents(_ *transportv1.WatchEventsRequest, stream transportv1.TransportService_WatchEventsServer) error {
	s.active.Add(1)
	defer s.active.Add(-1)
	<-stream.Context().Done()
	return stream.Context().Err()
}

type maintenanceLog struct {
	slog.Handler
	completed chan struct{}
}

func (h maintenanceLog) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "user operations scheduler completed" {
		select {
		case h.completed <- struct{}{}:
		default:
		}
	}
	return h.Handler.Handle(ctx, record)
}

func runtimeConfig(t *testing.T, options connection.Options) config.Config {
	t.Helper()
	values := map[string]string{
		"OCSERV_ENVIRONMENT": "test", "OCSERV_DATABASE_BACKEND": options.Backend,
		"OCSERV_DATABASE_URL": options.URL, "OCSERV_DATABASE_TLS_CA_FILE": options.CAFile,
		"OCSERV_AUDIT_EVENT_KEY_ID": "test-audit-event-v1", "OCSERV_TEST_AUDIT_EVENT_KEY_HEX": strings.Repeat("11", 32),
		"OCSERV_AUDIT_CHECKPOINT_KEY": strings.Repeat("22", 32),
		"OCSERV_TRANSPORT_UID":        strconv.Itoa(os.Geteuid()), "OCSERV_TRANSPORT_GID": strconv.Itoa(os.Getegid()),
	}
	cfg, err := config.Load(nil, func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Real Run, database and Go Worker/Owner/Trust loops. The UDS peer only supplies
// an idle event stream; negotiated fences are covered by the existing Rust E2E.
func TestControllerRoleLifecycleBackendIntegration(t *testing.T) {
	ownerOptions, runtimeOptions, account := controllerProcessDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ownerConfig := runtimeConfig(t, ownerOptions)
	ownerConfig.MigrateOnly, ownerConfig.RuntimeDBRole = true, account
	if err := Run(ctx, ownerConfig, BuildInfo{}, quietLogger()); err != nil {
		t.Fatal(err)
	}

	for _, role := range []config.Role{config.RoleAPI, config.RoleWorker, config.RoleScheduler, config.RoleAll} {
		t.Run(string(role), func(t *testing.T) {
			cfg := runtimeConfig(t, runtimeOptions)
			cfg.Role, cfg.ControllerEndpointID = role, strings.Repeat("ab", 32)
			dir := socketDirectory(t)
			cfg.TrustSocket, cfg.TransportSocket = filepath.Join(dir, "trust.sock"), filepath.Join(dir, "transport.sock")
			listener, err := net.Listen("unix", cfg.TransportSocket)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(cfg.TransportSocket, 0o660); err != nil {
				t.Fatal(err)
			}
			transport, grpcServer := &roleTransport{}, grpc.NewServer()
			transportv1.RegisterTransportServiceServer(grpcServer, transport)
			healthServer := health.NewServer()
			healthServer.SetServingStatus("ocserv.platform.transport.v1.TransportService", healthv1.HealthCheckResponse_SERVING)
			healthv1.RegisterHealthServer(grpcServer, healthServer)
			served := make(chan error, 1)
			go func() { served <- grpcServer.Serve(listener) }()
			defer func() { grpcServer.Stop(); listener.Close(); <-served }()
			cfg.HTTPAddress = e2eAddress(t)
			log := maintenanceLog{quietLogger().Handler(), make(chan struct{}, 1)}
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- Run(runCtx, cfg, BuildInfo{}, slog.New(log)) }()
			finished := false
			defer func() {
				stop()
				if !finished {
					select {
					case <-done:
					case <-time.After(15 * time.Second):
						t.Error("Run cleanup timed out")
					}
				}
			}()
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			ready, maintenance := false, false
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
				select {
				case err := <-done:
					finished = true
					t.Fatalf("role exited during assembly: %v", err)
				default:
				}
				select {
				case <-log.completed:
					maintenance = true
				default:
				}
				apiReady := !cfg.RunsAPI()
				if cfg.RunsAPI() {
					response, err := client.Get("http://" + cfg.HTTPAddress + "/readyz")
					if err == nil {
						apiReady = response.StatusCode == http.StatusOK
						response.Body.Close()
					}
				}
				workerReady := !cfg.RunsWorker() || transport.active.Load() == 2
				if apiReady && workerReady && (!cfg.RunsScheduler() || maintenance) {
					ready = true
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(20 * time.Millisecond):
				}
			}
			if !ready {
				t.Fatalf("role not active: streams=%d maintenance=%v", transport.active.Load(), maintenance)
			}
			_, trustErr := os.Stat(cfg.TrustSocket)
			if cfg.RunsWorker() != (trustErr == nil) {
				t.Fatalf("wrong connection owner/Trust role: %v", trustErr)
			}
			if !cfg.RunsWorker() && transport.active.Load() != 0 {
				t.Fatal("non-owner role started transport watcher")
			}
			stop()
			select {
			case err := <-done:
				finished = true
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("role did not stop")
			}
			if _, err := os.Stat(cfg.TrustSocket); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Trust socket survived Run", err)
			}
			if cfg.RunsAPI() {
				rebound, err := net.Listen("tcp", cfg.HTTPAddress)
				if err != nil {
					t.Fatal("HTTP port survived Run", err)
				}
				rebound.Close()
			}
		})
	}
	for _, failure := range []string{"late-construction", "HTTP-bind"} {
		t.Run(failure, func(t *testing.T) {
			cfg := runtimeConfig(t, runtimeOptions)
			cfg.Role, cfg.ControllerEndpointID = config.RoleAll, strings.Repeat("ab", 32)
			cfg.TrustSocket = filepath.Join(socketDirectory(t), "trust.sock")
			if failure == "late-construction" {
				cfg.CertificateSignerURL = "invalid-signer-url"
			} else {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				cfg.HTTPAddress = listener.Addr().String()
			}
			err := Run(ctx, cfg, BuildInfo{}, quietLogger())
			want := "configure external certificate signer"
			if failure == "HTTP-bind" {
				want = "serve HTTP"
			}
			if err == nil || !strings.Contains(err.Error(), want) || errors.Is(err, context.Canceled) {
				t.Fatalf("wrong failure cause: %v", err)
			}
			if _, err := os.Stat(cfg.TrustSocket); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial startup left Trust socket", err)
			}
		})
	}
}
