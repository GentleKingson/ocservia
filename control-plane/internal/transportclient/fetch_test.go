package transportclient

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// blockingFetchServer holds the unary response until the test releases the
// server's authorization decision.
type blockingFetchServer struct {
	transportv1.UnimplementedTransportServiceServer
	requested chan struct{}
	accept    chan struct{}
	reject    bool
}

func (s *blockingFetchServer) FetchArtifact(ctx context.Context, request *transportv1.FetchArtifactRequest) (*agentv1.ArtifactPayload, error) {
	close(s.requested)
	select {
	case <-s.accept:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.reject {
		return nil, status.Error(codes.PermissionDenied, "artifact grant rejected")
	}
	data := []byte("p12-fixture")
	digest := sha256.Sum256(data)
	return &agentv1.ArtifactPayload{ArtifactId: request.GetGrant().GetArtifactId(), Data: data, Sha256: digest[:]}, nil
}

func startFetchServer(t *testing.T, server transportv1.TransportServiceServer) string {
	t.Helper()
	directory := testutil.PrivateTempDir(t)
	path := filepath.Join(directory, "transport.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	transportv1.RegisterTransportServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	return path
}

func fetchTestClient(t *testing.T, path string) *Client {
	t.Helper()
	client, err := New(path, 5*time.Second, 1, uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fetchTestGrant(nodeID [16]byte) *agentv1.ArtifactGrantV1 {
	artifactID := uuid.Must(uuid.NewV7())
	grantID := uuid.Must(uuid.NewV7())
	return &agentv1.ArtifactGrantV1{NodeId: nodeID[:], ArtifactId: artifactID[:], GrantId: grantID[:], Purpose: "certificate_p12", MaxBytes: 256 << 10}
}

func TestFetchArtifactWaitsForServerAcceptance(t *testing.T) {
	server := &blockingFetchServer{requested: make(chan struct{}), accept: make(chan struct{})}
	client := fetchTestClient(t, startFetchServer(t, server))
	nodeID := uuid.Must(uuid.NewV7())
	var nodeBytes [16]byte
	copy(nodeBytes[:], nodeID[:])
	grant := fetchTestGrant(nodeBytes)

	fetched := make(chan struct{})
	var reader io.ReadCloser
	var fetchErr error
	go func() {
		reader, fetchErr = client.FetchArtifact(context.Background(), grant)
		close(fetched)
	}()
	select {
	case <-server.requested:
	case <-fetched:
		t.Fatalf("FetchArtifact returned before reaching the server: %v", fetchErr)
	case <-time.After(5 * time.Second):
		t.Fatal("FetchArtifact did not reach the server")
	}
	select {
	case <-fetched:
		t.Fatal("FetchArtifact returned before the server returned its response")
	case <-time.After(300 * time.Millisecond):
	}

	close(server.accept)
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchArtifact did not return after the server returned its response")
	}
	if fetchErr != nil {
		t.Fatalf("accepted fetch failed: %v", fetchErr)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read accepted artifact: %v", err)
	}
	if string(body) != "p12-fixture" {
		t.Fatalf("artifact body = %q", body)
	}
}

func TestFetchArtifactSurfacesAuthorizationFailureOnAcceptance(t *testing.T) {
	server := &blockingFetchServer{requested: make(chan struct{}), accept: make(chan struct{}), reject: true}
	client := fetchTestClient(t, startFetchServer(t, server))
	nodeID := uuid.Must(uuid.NewV7())
	var nodeBytes [16]byte
	copy(nodeBytes[:], nodeID[:])

	fetched := make(chan struct{})
	var reader io.ReadCloser
	var fetchErr error
	go func() {
		reader, fetchErr = client.FetchArtifact(context.Background(), fetchTestGrant(nodeBytes))
		close(fetched)
	}()
	select {
	case <-server.requested:
	case <-fetched:
		t.Fatalf("FetchArtifact returned before reaching the server: %v", fetchErr)
	case <-time.After(5 * time.Second):
		t.Fatal("FetchArtifact did not reach the server")
	}
	close(server.accept)
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchArtifact did not return after the server rejected the request")
	}
	if fetchErr == nil {
		_ = reader.Close()
		t.Fatal("rejected fetch produced a reader")
	}
	if status.Code(fetchErr) != codes.PermissionDenied {
		t.Fatalf("rejection = %v, want PermissionDenied", fetchErr)
	}
}
