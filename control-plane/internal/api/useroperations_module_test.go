package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/rbac"
	"github.com/GentleKingson/ocservia/control-plane/internal/useroperations"
	"github.com/google/uuid"
)

type blockedUserOperations struct {
	started  chan context.Context
	canceled chan error
	release  chan struct{}
}

func (p *blockedUserOperations) wait(ctx context.Context) error {
	p.started <- ctx
	<-ctx.Done()
	p.canceled <- ctx.Err()
	<-p.release
	return ctx.Err()
}
func (p *blockedUserOperations) GetPolicy(ctx context.Context, _ uuid.UUID, _ string) (useroperations.Policy, error) {
	return useroperations.Policy{}, p.wait(ctx)
}
func (p *blockedUserOperations) SetPolicy(ctx context.Context, _ useroperations.PolicyRequest) (useroperations.Policy, bool, error) {
	return useroperations.Policy{}, false, p.wait(ctx)
}
func (p *blockedUserOperations) CreateBatch(ctx context.Context, _ useroperations.BatchRequest) (useroperations.Batch, bool, error) {
	return useroperations.Batch{}, false, p.wait(ctx)
}
func (p *blockedUserOperations) GetBatch(ctx context.Context, _ uuid.UUID) (useroperations.Batch, error) {
	return useroperations.Batch{}, p.wait(ctx)
}
func (p *blockedUserOperations) Metrics(ctx context.Context, _ uuid.UUID) (useroperations.Metrics, error) {
	return useroperations.Metrics{}, p.wait(ctx)
}

func TestUserOperationsHTTPContextLifetime(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/nodes/" + baselineID + "/users/alice/policy", `{}`},
		{"GET", "/api/v1/nodes/" + baselineID + "/users/alice/policy", ""},
		{"POST", "/api/v1/user-batches", `{}`},
		{"GET", "/api/v1/user-batches/" + baselineID, ""},
		{"GET", "/api/v1/user-operations/metrics", ""},
	} {
		for _, cancelRequest := range []bool{false, true} {
			name := "deadline"
			if cancelRequest {
				name = "cancellation"
			}
			t.Run(route.method+route.path+"/"+name, func(t *testing.T) {
				timeout := 30 * time.Millisecond
				if cancelRequest {
					timeout = time.Second
				}
				s := NewBackend("127.0.0.1:0", nil, BuildInfo{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, timeout, true, "", 36)
				plans := &blockedUserOperations{started: make(chan context.Context, 1), canceled: make(chan error, 1), release: make(chan struct{})}
				var once sync.Once
				release := func() { once.Do(func() { close(plans.release) }) }
				t.Cleanup(func() { release(); _ = s.Shutdown(context.Background()) })
				s.userOpsHTTP.SetOperations(plans)
				s.userOpsHTTP.SetAuthorizer(rbac.NewBackend(nil))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					r := baselineRequest(route.method, route.path, strings.NewReader(route.body)).WithContext(ctx)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "context-test")
					w := httptest.NewRecorder()
					s.http.Handler.ServeHTTP(w, r)
					done <- w
				}()
				select {
				case got := <-plans.started:
					if _, ok := got.Deadline(); !ok {
						t.Fatal("HTTP deadline lost")
					}
					if got.Value(requestIDKey{}) != "baseline-request" {
						t.Fatal("request context lost")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("business method did not start")
				}
				wantErr := context.DeadlineExceeded
				if cancelRequest {
					cancel()
					wantErr = context.Canceled
				}
				select {
				case got := <-plans.canceled:
					if !errors.Is(got, wantErr) {
						t.Fatalf("service context: %v", got)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("service did not observe cancellation")
				}
				select {
				case w := <-done:
					if w.Code != 503 {
						t.Fatal("timeout status", w.Code)
					}
				case <-time.After(time.Second):
					t.Fatal("outer HTTP handler did not return")
				}
				shutdown, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancelShutdown()
				if err := s.Shutdown(shutdown); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown forgot UserOperations request: %v", err)
				}
				release()
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserOperationsRequestInfo(t *testing.T) {
	actor := auth.Principal{IdentityID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()), Issuer: auth.LocalIssuer, BreakGlass: true}
	workspaceID, approval := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, header := range []string{"", "bad", uuid.NewString(), " " + approval.String() + " "} {
		r := baselineRequest("POST", "/api/v1/user-batches", nil)
		ctx := context.WithValue(r.Context(), principalKey{}, actor)
		ctx = context.WithValue(ctx, workspaceKey{}, workspaceID)
		ctx = context.WithValue(ctx, requestIDKey{}, "original-request")
		r = r.WithContext(ctx)
		r.Header.Set("X-Approval-ID", header)
		r.Header.Set("X-Workspace-ID", uuid.NewString())
		info := userOperationsRequestInfo(r)
		wantApproval := uuid.Nil
		if strings.TrimSpace(header) == approval.String() {
			wantApproval = approval
		}
		if info.Principal != actor || info.WorkspaceID != workspaceID || info.ApprovalID != wantApproval || info.ActorID != actorID(r) || info.RequestID != "original-request" || info.Traceparent != requestTraceparent(r) {
			t.Fatalf("request information: %+v", info)
		}
	}
}
