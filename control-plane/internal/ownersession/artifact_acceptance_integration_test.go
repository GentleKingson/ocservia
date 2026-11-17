package ownersession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/database/postgres"
	"github.com/GentleKingson/ocservia/control-plane/internal/transportclient"
	"google.golang.org/grpc"
)

type artifactRegistryFence struct {
	mu       sync.Mutex
	failures bool
	fence    *agentv1.ConnectionFenceV2
}

func (r *artifactRegistryFence) RegisterOwnerFence(_ context.Context, fence *agentv1.ConnectionFenceV2) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures {
		return errors.New("transport unavailable")
	}
	r.fence = fence
	return nil
}

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

// TestManagerGuardSpansArtifactStreamAcceptanceIntegration pins the
// server-streaming acceptance boundary of the manager's artifact guard:
// the guard released by ExecuteFenced must stay engaged until transportd has
// verified the fence binding and accepted the fetch — proven by the first
// artifact frame — not merely until the client stream is created.
func TestManagerGuardSpansArtifactStreamAcceptanceIntegration(t *testing.T) {
	pool := testPool(t)
	signer, _ := testSigner(t)
	registry := &artifactRegistryFence{}
	manager, err := NewManagerBackend(postgres.WrapPool(pool), signer, registry, 30*time.Second, testLogger())
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

	artifactID := mustUUIDv7(t)
	grantID := mustUUIDv7(t)
	grant := &agentv1.ArtifactGrantV1{NodeId: nodeID[:], ArtifactId: artifactID[:], GrantId: grantID[:], Purpose: "certificate_p12", MaxBytes: 1 << 20}
	// Mirrors certificates.OpenArtifact: the guarded action only takes the
	// reader and returns; the artifact body itself is consumed outside the
	// guard after transportd has accepted the fetch.
	var reader io.ReadCloser
	fetchDone := make(chan error, 1)
	go func() {
		fetchDone <- manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, artifactID, FencingCapability,
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
	// the manager's guard must still pin the node lock.
	<-server.requested

	// A same-owner reopen must not advance the epoch while acceptance pends.
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
		t.Fatalf("manager artifact fetch through the accepting transport: %v", fetchErr)
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

	// The epoch advanced only after acceptance, leaving manager fence unavailable until reopened.
	var advancedEpoch int64
	if err := pool.QueryRow(context.Background(), `SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=$1`, nodeID[:]).Scan(&advancedEpoch); err != nil {
		t.Fatalf("read advanced epoch: %v", err)
	}
	if advancedEpoch != int64(firstFence.GetOwnerEpoch())+1 {
		t.Fatalf("authority epoch = %d, want exactly one advance to %d", advancedEpoch, int64(firstFence.GetOwnerEpoch())+1)
	}
	ran := false
	if err := manager.ExecuteFenced(context.Background(), nodeID, agentv1.FenceOperationKind_FENCE_OPERATION_KIND_ARTIFACT, mustUUIDv7(t), FencingCapability,
		func(context.Context, *agentv1.ConnectionFenceV2, *agentv1.FenceBindingV2) error {
			ran = true
			return nil
		}); !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("manager artifact bind on the failed reopen = %v, want ErrFenceUnavailable", err)
	}
	if ran {
		t.Fatal("manager ran an artifact mutation after failed reopen")
	}
}
