package transportclient

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const maxMessageBytes = 1 << 20

type CursorStore interface {
	LastEventID(context.Context) ([]byte, error)
}

type EventHandler interface {
	Ingest(context.Context, *transportv1.TransportEvent) error
}

type GapReconciler interface {
	ReconcileEventGap(context.Context) error
}

type Client struct {
	path          string
	deadline      time.Duration
	queueCapacity int
}

func New(path string, deadline time.Duration, queueCapacity int) (*Client, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("transport socket path must be absolute")
	}
	if deadline <= 0 || queueCapacity < 1 || queueCapacity > 4096 {
		return nil, errors.New("transport deadline and queue capacity are invalid")
	}
	return &Client{path: path, deadline: deadline, queueCapacity: queueCapacity}, nil
}

func (c *Client) SendCommand(ctx context.Context, nodeID, envelope []byte) error {
	ctx, span := otel.Tracer("ocservia.transportclient").Start(ctx, "transport.uds.send", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	if len(envelope) == 0 || len(envelope) > maxMessageBytes {
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
			reconcileErr := reconciler.ReconcileEventGap(reconcileCtx)
			cancel()
			if reconcileErr != nil {
				return fmt.Errorf("reconcile transport event gap: %w", reconcileErr)
			}
			reconcileCursor = true
			continue
		}
		reconcileCursor = false
		delay := fullJitter(attempt, 100*time.Millisecond, 5*time.Second)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	info, err := os.Stat(filepath.Dir(c.path))
	if err != nil {
		return nil, fmt.Errorf("inspect transport socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o002 != 0 {
		return nil, errors.New("transport socket directory is not controlled")
	}
	target := "passthrough:///transportd"
	connection, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", c.path)
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
