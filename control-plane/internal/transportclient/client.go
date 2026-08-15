package transportclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/udssecurity"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const (
	maxPayloadBytes = 1 << 20
	maxMessageBytes = maxPayloadBytes + (4 << 10)
)

type CursorStore interface {
	LastEventID(context.Context) ([]byte, error)
}

type EventHandler interface {
	Ingest(context.Context, *transportv1.TransportEvent) error
}

type GapReconciler interface {
	ReconcileEventGap(context.Context, func(context.Context, []byte) (bool, error)) error
}

type Client struct {
	path          string
	deadline      time.Duration
	queueCapacity int
	expectedUID   uint32
	expectedGID   uint32
}

func New(path string, deadline time.Duration, queueCapacity int, expectedUID, expectedGID uint32) (*Client, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("transport socket path must be absolute")
	}
	if deadline <= 0 || queueCapacity < 1 || queueCapacity > 4096 {
		return nil, errors.New("transport deadline and queue capacity are invalid")
	}
	return &Client{path: filepath.Clean(path), deadline: deadline, queueCapacity: queueCapacity, expectedUID: expectedUID, expectedGID: expectedGID}, nil
}

func (c *Client) SendCommand(ctx context.Context, nodeID, envelope []byte) error {
	ctx, span := otel.Tracer("ocservia.transportclient").Start(ctx, "transport.uds.send", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	if len(envelope) == 0 || len(envelope) > maxPayloadBytes {
		return errors.New("command envelope size is invalid")
	}
	connection, err := c.dial()
	if err != nil {
		return err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	response, err := transportv1.NewTransportServiceClient(connection).SendCommand(rpcCtx, &transportv1.SendCommandRequest{NodeId: nodeID, CommandEnvelope: envelope})
	if err != nil {
		return fmt.Errorf("send simulator command: %w", err)
	}
	if !response.GetAccepted() {
		return errors.New("transport rejected simulator command")
	}
	return nil
}

func (c *Client) FetchArtifact(ctx context.Context, grant *agentv1.ArtifactGrantV1, fenceBinding *agentv1.FenceBindingV2) (io.ReadCloser, error) {
	if grant == nil || len(grant.GetNodeId()) != 16 || len(grant.GetArtifactId()) != 16 || grant.GetMaxBytes() < 1 || grant.GetMaxBytes() > 64<<20 {
		return nil, errors.New("artifact request is invalid")
	}
	nodeID, err := uuid.FromBytes(grant.GetNodeId())
	if err != nil {
		return nil, errors.New("artifact node is invalid")
	}
	artifactID, err := uuid.FromBytes(grant.GetArtifactId())
	if err != nil {
		return nil, errors.New("artifact ID is invalid")
	}
	maxBytes := int64(grant.GetMaxBytes())
	connection, err := c.dial()
	if err != nil {
		return nil, err
	}
	stream, err := transportv1.NewTransportServiceClient(connection).FetchArtifact(ctx, &transportv1.FetchArtifactRequest{NodeId: nodeID[:], ArtifactId: artifactID[:], Purpose: "certificate_p12", MaxBytes: uint64(maxBytes), Grant: grant, FenceBinding: fenceBinding})
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("fetch artifact: %w", err)
	}
	// transportd accepts a fetch only after its handler verified the fence
	// binding; a rejected fetch answers with a status error instead of
	// frames. Waiting for the first validated artifact frame therefore keeps
	// the caller's ownership guard engaged across fence verification and
	// stream acceptance — and fails the call itself when transportd rejects —
	// without holding the guard for the whole artifact transfer.
	reader, writer := io.Pipe()
	accepted := make(chan error, 1)
	go func() {
		defer connection.Close()
		hash := sha256.New()
		var offset int64
		reportAcceptance := func(err error) {
			select {
			case accepted <- err:
			default:
			}
		}
		for {
			chunk, receiveErr := stream.Recv()
			if receiveErr != nil {
				_ = writer.CloseWithError(receiveErr)
				reportAcceptance(receiveErr)
				return
			}
			if !bytes.Equal(chunk.GetArtifactId(), artifactID[:]) || chunk.GetOffset() != uint64(offset) || len(chunk.GetData()) > 256<<10 || offset+int64(len(chunk.GetData())) > maxBytes {
				inconsistent := errors.New("artifact stream is inconsistent")
				_ = writer.CloseWithError(inconsistent)
				reportAcceptance(inconsistent)
				return
			}
			if _, writeErr := hash.Write(chunk.GetData()); writeErr != nil {
				_ = writer.CloseWithError(writeErr)
				reportAcceptance(writeErr)
				return
			}
			offset += int64(len(chunk.GetData()))
			// The first validated frame proves transportd accepted the fetch;
			// signal before the pipe write, which blocks until the caller
			// consumes the returned reader.
			reportAcceptance(nil)
			if _, writeErr := writer.Write(chunk.GetData()); writeErr != nil {
				_ = writer.CloseWithError(writeErr)
				reportAcceptance(writeErr)
				return
			}
			if chunk.GetEof() {
				if offset == 0 || len(chunk.GetSha256()) != sha256.Size || !bytes.Equal(hash.Sum(nil), chunk.GetSha256()) {
					digestMismatch := errors.New("artifact digest mismatch")
					_ = writer.CloseWithError(digestMismatch)
					reportAcceptance(digestMismatch)
					return
				}
				_ = writer.Close()
				return
			}
		}
	}()
	if err := <-accepted; err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("fetch artifact acceptance: %w", err)
	}
	return reader, nil
}

func (c *Client) ConsumeArtifact(ctx context.Context, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, fenceBinding *agentv1.FenceBindingV2) error {
	consumed, err := c.consumeArtifact(ctx, grant, digest, size, false, fenceBinding)
	if err != nil {
		return err
	}
	if !consumed {
		return errors.New("transport did not consume artifact")
	}
	return nil
}

func (c *Client) ConfirmArtifactConsumed(ctx context.Context, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, fenceBinding *agentv1.FenceBindingV2) (bool, error) {
	return c.consumeArtifact(ctx, grant, digest, size, true, fenceBinding)
}

func (c *Client) consumeArtifact(ctx context.Context, grant *agentv1.ArtifactGrantV1, digest []byte, size int64, confirmOnly bool, fenceBinding *agentv1.FenceBindingV2) (bool, error) {
	if grant == nil || len(grant.GetNodeId()) != 16 || len(grant.GetArtifactId()) != 16 || len(grant.GetGrantId()) != 16 || len(digest) != sha256.Size || size < 1 || uint64(size) != grant.GetMaxBytes() {
		return false, errors.New("artifact consumption request is invalid")
	}
	connection, err := c.dial()
	if err != nil {
		return false, err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	response, err := transportv1.NewTransportServiceClient(connection).ConsumeArtifact(rpcCtx, &transportv1.ConsumeArtifactRequest{
		NodeId:       grant.GetNodeId(),
		Grant:        grant,
		Sha256:       bytes.Clone(digest),
		Size:         uint64(size),
		ConfirmOnly:  confirmOnly,
		FenceBinding: fenceBinding,
	})
	if err != nil {
		return false, fmt.Errorf("consume artifact: %w", err)
	}
	return response.GetConsumed(), nil
}

func (c *Client) RunWatch(ctx context.Context, cursors CursorStore, handler EventHandler) error {
	reconcileCursor := false
	for attempt := uint(0); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.watchOnce(ctx, cursors, handler, reconcileCursor)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.Unauthenticated {
			return err
		}
		if status.Code(err) == codes.OutOfRange {
			reconciler, ok := handler.(GapReconciler)
			if !ok {
				return errors.New("transport event gap cannot be reconciled")
			}
			reconcileCtx, cancel := context.WithTimeout(ctx, c.deadline)
			reconcileErr := reconciler.ReconcileEventGap(reconcileCtx, c.NodeConnected)
			cancel()
			if reconcileErr != nil {
				if err := waitBackoff(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			reconcileCursor = true
			continue
		}
		reconcileCursor = false
		if err := waitBackoff(ctx, attempt); err != nil {
			return err
		}
	}
}

func waitBackoff(ctx context.Context, attempt uint) error {
	delay := fullJitter(attempt, 100*time.Millisecond, 5*time.Second)
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) NodeConnected(ctx context.Context, nodeID []byte) (bool, error) {
	connection, err := c.dial()
	if err != nil {
		return false, err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	_, err = transportv1.NewTransportServiceClient(connection).GetNodeConnection(rpcCtx, &transportv1.GetNodeConnectionRequest{NodeId: nodeID})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get transport node connection: %w", err)
	}
	return true, nil
}

// UpdateNodeTrust applies one trust update. operationID is the canonical
// identity of the exact update carrier; it must be the identity the fence
// binding was signed over.
func (c *Client) UpdateNodeTrust(ctx context.Context, nodeID, endpointID []byte, state transportv1.NodeTrustState, reason string, revision uint64, operationID []byte, fenceBinding *agentv1.FenceBindingV2) error {
	if len(operationID) != 0 && len(operationID) != 16 {
		return errors.New("trust update operation identity is invalid")
	}
	connection, err := c.dial()
	if err != nil {
		return err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	response, err := transportv1.NewTransportServiceClient(connection).UpdateNodeTrust(rpcCtx, &transportv1.UpdateNodeTrustRequest{NodeId: nodeID, EndpointId: endpointID, State: state, Reason: reason, Revision: revision, OperationId: operationID, FenceBinding: fenceBinding})
	if err != nil {
		return fmt.Errorf("update transport node trust: %w", err)
	}
	if response.GetDisposition() != transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_APPLIED || response.GetRetainedRevision() != revision || response.GetRetainedState() != state {
		return fmt.Errorf("transport retained trust state %s at revision %d with disposition %s", response.GetRetainedState(), response.GetRetainedRevision(), response.GetDisposition())
	}
	return nil
}

func (c *Client) CloseNode(ctx context.Context, nodeID []byte, reason string, fenceBinding *agentv1.FenceBindingV2) error {
	connection, err := c.dial()
	if err != nil {
		return err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	_, err = transportv1.NewTransportServiceClient(connection).CloseNode(rpcCtx, &transportv1.CloseNodeRequest{NodeId: nodeID, Reason: reason, FenceBinding: fenceBinding})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("close transport node: %w", err)
	}
	return nil
}

// RegisterOwnerFence pushes a Controller-signed owner fence to transportd,
// which verifies it, records the term, and retires a superseded owner's
// mutation session when the epoch increases.
func (c *Client) RegisterOwnerFence(ctx context.Context, fence *agentv1.ConnectionFenceV2) error {
	if fence == nil || len(fence.GetNodeId()) != 16 || len(fence.GetFenceId()) != 16 || fence.GetOwnerEpoch() == 0 {
		return errors.New("owner fence is invalid")
	}
	connection, err := c.dial()
	if err != nil {
		return err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	response, err := transportv1.NewTransportServiceClient(connection).RegisterOwnerFence(rpcCtx, &transportv1.RegisterOwnerFenceRequest{Fence: fence})
	if err != nil {
		return fmt.Errorf("register owner fence: %w", err)
	}
	if response.GetDisposition() != transportv1.OwnerFenceDisposition_OWNER_FENCE_DISPOSITION_APPLIED &&
		response.GetDisposition() != transportv1.OwnerFenceDisposition_OWNER_FENCE_DISPOSITION_REFRESHED {
		return fmt.Errorf("transport rejected owner fence with disposition %s at epoch %d", response.GetDisposition(), response.GetRetainedEpoch())
	}
	return nil
}

// GetOwnerFence returns the fence transportd registered for the node, or nil
// when transportd has none. The returned fence was signature-verified by
// transportd before registration.
func (c *Client) GetOwnerFence(ctx context.Context, nodeID []byte) (*agentv1.ConnectionFenceV2, error) {
	if len(nodeID) != 16 {
		return nil, errors.New("owner fence node is invalid")
	}
	connection, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rpcCtx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	response, err := transportv1.NewTransportServiceClient(connection).GetOwnerFence(rpcCtx, &transportv1.GetOwnerFenceRequest{NodeId: nodeID})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get owner fence: %w", err)
	}
	return response.GetFence(), nil
}

func (c *Client) watchOnce(ctx context.Context, cursors CursorStore, handler EventHandler, reconcileCursor bool) error {
	connection, err := c.dial()
	if err != nil {
		return err
	}
	defer connection.Close()
	healthCtx, healthCancel := context.WithTimeout(ctx, c.deadline)
	health, err := healthv1.NewHealthClient(connection).Check(healthCtx, &healthv1.HealthCheckRequest{Service: "ocserv.platform.transport.v1.TransportService"})
	healthCancel()
	if err != nil {
		return fmt.Errorf("check transport health: %w", err)
	}
	if health.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		return errors.New("transport health is not serving")
	}
	var cursor []byte
	if !reconcileCursor {
		cursorCtx, cancel := context.WithTimeout(ctx, c.deadline)
		cursor, err = cursors.LastEventID(cursorCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("restore transport cursor: %w", err)
		}
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	stream, err := transportv1.NewTransportServiceClient(connection).WatchEvents(streamCtx, &transportv1.WatchEventsRequest{AfterEventId: cursor}, grpc.MaxCallRecvMsgSize(maxMessageBytes))
	if err != nil {
		return fmt.Errorf("watch transport events: %w", err)
	}
	events := make(chan *transportv1.TransportEvent, c.queueCapacity)
	consumerErr := make(chan error, 1)
	go func() {
		for event := range events {
			eventCtx, eventCancel := context.WithTimeout(streamCtx, c.deadline)
			err := handler.Ingest(eventCtx, event)
			eventCancel()
			if err != nil {
				consumerErr <- err
				streamCancel()
				return
			}
		}
		consumerErr <- nil
	}()
	defer close(events)
	for {
		event, err := stream.Recv()
		if err != nil {
			select {
			case ingestErr := <-consumerErr:
				if ingestErr != nil {
					return fmt.Errorf("ingest transport event: %w", ingestErr)
				}
			default:
			}
			return fmt.Errorf("receive transport event: %w", err)
		}
		select {
		case events <- event:
		case err := <-consumerErr:
			return fmt.Errorf("ingest transport event: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) dial() (*grpc.ClientConn, error) {
	target := "passthrough:///transportd"
	connection, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			identity, err := udssecurity.ValidateSocket(c.path, c.expectedUID, c.expectedGID, 0o660)
			if err != nil {
				return nil, fmt.Errorf("validate transport socket: %w", err)
			}
			var dialer net.Dialer
			connection, err := dialer.DialContext(ctx, "unix", c.path)
			if err != nil {
				return nil, err
			}
			if err := udssecurity.RequirePeerUID(connection, c.expectedUID); err != nil {
				_ = connection.Close()
				return nil, err
			}
			if err := udssecurity.SameSocket(c.path, identity); err != nil {
				_ = connection.Close()
				return nil, err
			}
			return connection, nil
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes), grpc.MaxCallSendMsgSize(maxMessageBytes)),
	)
	if err != nil {
		return nil, fmt.Errorf("create UDS gRPC client: %w", err)
	}
	return connection, nil
}

func fullJitter(attempt uint, base, maximum time.Duration) time.Duration {
	shift := attempt
	if shift > 16 {
		shift = 16
	}
	ceiling := base * time.Duration(1<<shift)
	if ceiling > maximum || ceiling < base {
		ceiling = maximum
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)+1))
	if err != nil {
		return ceiling
	}
	return time.Duration(value.Int64())
}
