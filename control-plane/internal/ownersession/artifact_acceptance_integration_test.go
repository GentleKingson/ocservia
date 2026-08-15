package ownersession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"google.golang.org/grpc"
)

// acceptanceTransportServer mimics transportd's FetchArtifact acceptance
// boundary over a real gRPC endpoint: the request arrives, the fence
// verification the production handler performs would run here, and only
// after the test releases the gate does the handler return — emitting the
// response headers and first artifact frame the client's ownership guard
// waits for.
type acceptanceTransportServer struct {
	transportv1.UnimplementedTransportServiceServer
	requested chan struct{}
	accept    chan struct{}
}

func (s *acceptanceTransportServer) FetchArtifact(request *transportv1.FetchArtifactRequest, stream grpc.ServerStreamingServer[agentv1.ArtifactChunk]) error {
	close(s.requested)
	<-s.accept
	data := []byte("p12-fixture")
	digest := sha256.Sum256(data)
	return stream.Send(&agentv1.ArtifactChunk{ArtifactId: request.GetArtifactId(), Data: data, Eof: true, Sha256: digest[:]})
}

// TestObserverGuardSpansArtifactStreamAcceptanceIntegration pins the
// server-streaming acceptance boundary of the observer's artifact guard:
// the guard released by ExecuteFenced must stay engaged until transportd has
// verified the fence binding and accepted the fetch — proven by the first
// artifact frame — not merely until the client stream is created. While the
// transport server withholds acceptance, PostgreSQL must refuse to advance
// the node epoch; once it accepts, a same-owner reopen with a failing
// registration advances the epoch and leaves the stale observer path closed
// until a successor's real Acquire reopens it.
func TestObserverGuardSpansArtifactStreamAcceptanceIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registry := &registryFence{}
	manager, err := NewManager(pool, signer, registry, 30*time.Second, testLogger())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	nodeID, endpointID := testNodeAndEndpoint(t)
	firstFence, err := manager.OpenSession(context.Background(), nodeID, endpointID, 61, []string{FencingCapability})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}

	server := &acceptanceTransportServer{requested: make(chan struct{}), accept: make(chan struct{})}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(filepath.Join(workingDirectory, "..", ".."), ".os-fetch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	transportv1.RegisterTransportServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	transport, err := transportclient.New(socketPath, 5*time.Second, 1, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatalf("new transport client: %v", err)
	}

	observer, err := NewObserver(pool, registry, signer)
	if err != nil {
		t.Fatalf("new observer: %v", err)
	}
	artifactID := mustUUIDv7(t)
	grantID := mustUUIDv7(t)
	grant := &agentv1.ArtifactGrantV1{NodeId: nodeID[:], ArtifactId: artifactID[:], GrantId: grantID[:], Purpose: "certificate_p12", MaxBytes: 1 << 20}
	// Mirrors certificates.OpenArtifact: the guarded action only takes the
	// reader and returns; the artifact body itself is consumed outside the
	// guard after transportd has accepted the fetch.
	var reader io.ReadCloser
	fetchDone := make(chan error, 1)
	go func() {
		fetchDone <- observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, artifactID, FencingCapability,
			func(ctx context.Context, _ *agentv1.ConnectionFenceV2, binding *agentv1.FenceBindingV2) error {
				fetched, fetchErr := transport.FetchArtifact(ctx, grant, binding)
				if fetchErr != nil {
					return fetchErr
				}
				reader = fetched
				return nil
			})
	}()
	// The request reached the transport server; acceptance is withheld, so
	// the observer's guard must still pin the authority row.
	<-server.requested

	// A same-owner reopen — whose registration will fail, keeping transportd
	// on the old fence — must not advance the epoch while acceptance pends.
	registry.failures = true
	reopenDone := make(chan error, 1)
	go func() {
		_, reopenErr := manager.OpenSession(context.Background(), nodeID, endpointID, 62, []string{FencingCapability})
		reopenDone <- reopenErr
	}()
	select {
	case reopenErr := <-reopenDone:
		t.Fatalf("Acquire advanced while the artifact stream was still unaccepted: %v", reopenErr)
	case <-time.After(400 * time.Millisecond):
	}

	// The transport server completes verification and accepts the fetch; the
	// guard may only now release, which lets the withheld Acquire proceed.
	close(server.accept)
	if fetchErr := <-fetchDone; fetchErr != nil {
		t.Fatalf("observer artifact fetch through the accepting transport: %v", fetchErr)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read accepted artifact outside the guard: %v", err)
	}
	if !bytes.Equal(body, []byte("p12-fixture")) {
		t.Fatalf("artifact body = %q", body)
	}
	select {
	case reopenErr := <-reopenDone:
		if reopenErr == nil {
			t.Fatal("reopen through a failing registrar unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not proceed after the guard was released")
	}

	// The epoch advanced only after acceptance, and transportd's registry
	// kept serving the old term: the stale observer path stays closed.
	var advancedEpoch int64
	if err := pool.QueryRow(context.Background(), `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1`, nodeID[:]).Scan(&advancedEpoch); err != nil {
		t.Fatalf("read advanced epoch: %v", err)
	}
	if advancedEpoch != int64(firstFence.GetOwnerEpoch())+1 {
		t.Fatalf("authority epoch = %d, want exactly one advance to %d", advancedEpoch, int64(firstFence.GetOwnerEpoch())+1)
	}
	if registered, readErr := registry.GetOwnerFence(context.Background(), nodeID[:]); readErr != nil || registered.GetOwnerEpoch() != firstFence.GetOwnerEpoch() {
		t.Fatalf("registry after the failed reopen = (%v, epoch %d), want the stale term %d", readErr, registered.GetOwnerEpoch(), firstFence.GetOwnerEpoch())
	}
	ran := false
	if err := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, mustUUIDv7(t), FencingCapability,
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("observer artifact bind on the stale registered term = %v, want ErrNotOwner", err)
	}
	if ran {
		t.Fatal("observer ran an artifact mutation for the stale registered term")
	}

	// A successor's real Acquire with a successfully registered higher epoch
	// reopens the observer path on exactly the successor's term.
	registry.failures = false
	successor, err := NewManager(pool, signer, registry, 30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new successor manager: %v", err)
	}
	successorFence, err := successor.OpenSession(context.Background(), nodeID, endpointID, 12, []string{FencingCapability})
	if err != nil {
		t.Fatalf("successor take over after the accepted fetch: %v", err)
	}
	if int64(successorFence.GetOwnerEpoch()) <= advancedEpoch {
		t.Fatalf("successor epoch %d does not exceed the advanced epoch %d", successorFence.GetOwnerEpoch(), advancedEpoch)
	}
	if err := observer.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, mustUUIDv7(t), FencingCapability,
		func(_ context.Context, fence *agentv1.ConnectionFenceV2, _ *agentv1.FenceBindingV2) error {
			if fence.GetOwnerEpoch() != successorFence.GetOwnerEpoch() {
				t.Fatalf("observer bind after the takeover = epoch %d, want %d", fence.GetOwnerEpoch(), successorFence.GetOwnerEpoch())
			}
			return nil
		}); err != nil {
		t.Fatalf("observer artifact bind after the successor takeover: %v", err)
	}
}
