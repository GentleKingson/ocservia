package trustserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/enrollment"
	"github.com/GentleKingson/ocservia/control-plane/internal/udssecurity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Handler struct {
	transportv1.UnimplementedTrustServiceServer
	service *enrollment.Service
}

func NewHandler(service *enrollment.Service) *Handler { return &Handler{service: service} }

func (h *Handler) CheckEndpoint(ctx context.Context, request *transportv1.CheckEndpointRequest) (*transportv1.CheckEndpointResponse, error) {
	permitted, err := h.service.CheckEndpoint(ctx, request)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "trust authority unavailable")
	}
	return &transportv1.CheckEndpointResponse{Permitted: permitted}, nil
}

func (h *Handler) Enroll(ctx context.Context, request *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	response, err := h.service.Enroll(ctx, request)
	switch {
	case errors.Is(err, enrollment.ErrInvalidToken), errors.Is(err, enrollment.ErrEndpointMismatch), errors.Is(err, enrollment.ErrEndpointProof):
		return nil, status.Error(codes.PermissionDenied, "enrollment rejected")
	case errors.Is(err, enrollment.ErrPendingLimit):
		return nil, status.Error(codes.ResourceExhausted, "pending enrollment capacity reached")
	case errors.Is(err, enrollment.ErrInvalidRequest):
		return nil, status.Error(codes.InvalidArgument, "enrollment request rejected")
	case err != nil:
		return nil, status.Error(codes.Unavailable, "trust authority unavailable")
	default:
		return response, nil
	}
}

func (h *Handler) AuthorizeSession(ctx context.Context, request *transportv1.AuthorizeSessionRequest) (*agentv1.SessionHandshakeResponse, error) {
	response, err := h.service.AuthorizeSession(ctx, request)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "trust authority unavailable")
	}
	return response, nil
}

type Server struct {
	grpc     *grpc.Server
	listener net.Listener
	path     string
	identity socketIdentity
}

func New(path string, handler *Handler, transportUID uint32) (*Server, error) {
	listener, identity, err := listen(path)
	if err != nil {
		return nil, err
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(64<<10), grpc.MaxSendMsgSize(64<<10))
	transportv1.RegisterTrustServiceServer(server, handler)
	listener = &credentialListener{Listener: listener, expectedUID: transportUID}
	return &Server{grpc: server, listener: listener, path: listener.Addr().String(), identity: identity}, nil
}

type credentialListener struct {
	net.Listener
	expectedUID uint32
}

func (l *credentialListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := udssecurity.RequirePeerUID(connection, l.expectedUID); err == nil {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func (s *Server) Serve() error {
	if err := s.grpc.Serve(s.listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
	closeErr := s.listener.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}
	removeErr := removeSocket(s.path, s.identity)
	return errors.Join(closeErr, removeErr)
}

type socketIdentity struct{ device, inode uint64 }

func listen(path string) (net.Listener, socketIdentity, error) {
	if !filepath.IsAbs(path) {
		return nil, socketIdentity{}, errors.New("trust socket path must be absolute")
	}
	path, err := udssecurity.ValidateParent(path, uint32(os.Geteuid()))
	if err != nil {
		return nil, socketIdentity{}, fmt.Errorf("validate trust socket ancestry: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, socketIdentity{}, errors.New("refusing to replace non-socket trust path")
		}
		if _, err := udssecurity.ValidateSocket(path, uint32(os.Geteuid()), uint32(os.Getegid()), 0o660); err != nil {
			return nil, socketIdentity{}, fmt.Errorf("validate existing trust socket: %w", err)
		}
		probe, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return nil, socketIdentity{}, errors.New("trust socket is already active")
		}
		if !staleSocketError(dialErr) {
			return nil, socketIdentity{}, fmt.Errorf("probe existing trust socket: %w", dialErr)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, socketIdentity{}, fmt.Errorf("remove stale trust socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, socketIdentity{}, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, socketIdentity{}, fmt.Errorf("listen on trust socket: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, socketIdentity{}, err
	}
	if _, err := udssecurity.ValidateSocket(path, uint32(os.Geteuid()), uint32(os.Getegid()), 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, socketIdentity{}, err
	}
	identity, err := socketID(path)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, socketIdentity{}, err
	}
	return listener, identity, nil
}

func staleSocketError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)
}

func socketID(path string) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, errors.New("socket identity unavailable")
	}
	return socketIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func removeSocket(path string, expected socketIdentity) error {
	actual, err := socketID(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("trust socket path changed during shutdown")
	}
	return os.Remove(path)
}
