package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/configplan"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	"github.com/GentleKingson/ocservia/control-plane/internal/eventstream"
	"github.com/GentleKingson/ocservia/control-plane/internal/telemetry"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
)

func testHTTPConfig(devAuth bool) HTTPConfig {
	return HTTPConfig{
		Address: "127.0.0.1:0", BodyLimit: 1 << 20, RequestTimeout: 15 * time.Second,
		DevAuth: devAuth, BrowserOrigin: authTestOrigin, EventStreams: eventstream.DefaultConfig(),
	}
}

func newTestServer(t *testing.T, config HTTPConfig, backend database.Backend, modules Modules, authorization Authorization) *Server {
	t.Helper()
	s, err := NewServer(config, backend, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), modules, authorization)
	testutil.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestHTTPConstructionTypedNil(t *testing.T) {
	var nodes *telemetry.Service
	var plans *configplan.Service
	var ops *useroperations.Service
	s := newTestServer(t, testHTTPConfig(true), nil, Modules{Nodes: nodes, ConfigPlans: plans, UserOperations: ops}, Authorization{})
	if s.nodes != nil || s.configPlans != nil || s.userOperations != nil {
		t.Fatal("typed nil retained")
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/nodes"},
		{"GET", "/api/v1/config-plans/" + baselineID},
		{"POST", "/api/v1/nodes/" + baselineID + "/config-plans"},
		{"GET", "/api/v1/nodes/" + baselineID + "/users/alice/policy"},
	} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, baselineRequest(route.method, route.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body)
		}
	}
}
