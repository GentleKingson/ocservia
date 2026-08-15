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
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// blockingFetchServer is a stand-in for transportd whose FetchArtifact
// handler mimics the production acceptance boundary: the request arrives,
// fence verification would run, and only afterwards does the handler return
// — at which point gRPC emits the response headers the client waits on.
type blockingFetchServer struct {
	transportv1.UnimplementedTransportServiceServer
	requested chan struct{}
	accept    chan struct{}
	reject    bool
}

func (s *blockingFetchServer) FetchArtifact(request *transportv1.FetchArtifactRequest, stream grpc.ServerStreamingServer[agentv1.ArtifactChunk]) error {
	close(s.requested)
	<-s.accept
	if s.reject {
		return status.Error(codes.PermissionDenied, "fence binding rejected")
	}
	data := []byte("p12-fixture")
	digest := sha256.Sum256(data)
	return stream.Send(&agentv1.ArtifactChunk{ArtifactId: request.GetArtifactId(), Data: data, Eof: true, Sha256: digest[:]})
}

func startFetchServer(t *testing.T, server *blockingFetchServer) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(filepath.Join(workingDirectory, "..", ".."), ".tc-fetch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
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
	return &agentv1.ArtifactGrantV1{NodeId: nodeID[:], ArtifactId: artifactID[:], GrantId: grantID[:], Purpose: "certificate_p12", MaxBytes: 1 << 20}
}

// TestFetchArtifactWaitsForServerAcceptance pins the streaming acceptance
// contract: FetchArtifact may not hand out its reader until the server has
// authorized the fetch and emitted response headers. Returning on client
// stream creation alone would release the caller's ownership guard before
// transportd ran its fence verification.
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
		reader, fetchErr = client.FetchArtifact(context.Background(), grant, nil)
		close(fetched)
	}()
	<-server.requested
	select {
	case <-fetched:
		t.Fatal("FetchArtifact returned before the server accepted the stream; the ownership guard would be released before transportd verified the fence binding")
	case <-time.After(300 * time.Millisecond):
	}

	close(server.accept)
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchArtifact did not return after the server accepted the stream")
	}
	if fetchErr != nil {
		t.Fatalf("accepted fetch failed: %v", fetchErr)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read accepted artifact: %v", err)
	}
	if string(body) != "p12-fixture" {
		t.Fatalf("artifact body = %q", body)
	}
}

// TestFetchArtifactSurfacesAuthorizationFailureOnAcceptance proves a fetch
// the server rejects during fence verification fails the FetchArtifact call
// itself instead of producing a reader that errors asynchronously.
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
		reader, fetchErr = client.FetchArtifact(context.Background(), fetchTestGrant(nodeBytes), nil)
		close(fetched)
	}()
	<-server.requested
	close(server.accept)
	<-fetched
	if fetchErr == nil {
		_ = reader.Close()
		t.Fatal("rejected fetch produced a reader")
	}
	if status.Code(fetchErr) != codes.PermissionDenied {
		t.Fatalf("rejection = %v, want PermissionDenied", fetchErr)
	}
}
