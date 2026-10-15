package transportclient

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type trustRPCServer struct {
	transportv1.UnimplementedTransportServiceServer
	update    func(context.Context) (*transportv1.UpdateNodeTrustResponse, error)
	closeNode func(context.Context) error
}

func (s *trustRPCServer) UpdateNodeTrust(ctx context.Context, _ *transportv1.UpdateNodeTrustRequest) (*transportv1.UpdateNodeTrustResponse, error) {
	return s.update(ctx)
}

func (s *trustRPCServer) CloseNode(ctx context.Context, _ *transportv1.CloseNodeRequest) (*transportv1.CloseNodeResponse, error) {
	return &transportv1.CloseNodeResponse{}, s.closeNode(ctx)
}

func TestTrustUpdateRequiresExactAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition transportv1.TrustUpdateDisposition
		revision    uint64
		state       transportv1.NodeTrustState
		accepted    bool
	}{
		{"exact", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_APPLIED, 3, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, true},
		{"older", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_APPLIED, 2, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, false},
		{"newer", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_APPLIED, 4, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, false},
		{"wrong-state", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_APPLIED, 3, transportv1.NodeTrustState_NODE_TRUST_STATE_ACTIVE, false},
		{"stale", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_STALE, 3, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, false},
		{"rejected", transportv1.TrustUpdateDisposition_TRUST_UPDATE_DISPOSITION_REJECTED, 3, transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &trustRPCServer{update: func(context.Context) (*transportv1.UpdateNodeTrustResponse, error) {
				return &transportv1.UpdateNodeTrustResponse{Disposition: test.disposition, RetainedRevision: test.revision, RetainedState: test.state}, nil
			}}
			client, err := New(startFetchServer(t, server), time.Second, 1, uint32(os.Geteuid()), uint32(os.Getegid()))
			if err != nil {
				t.Fatal(err)
			}
			err = client.UpdateNodeTrust(context.Background(), make([]byte, 16), make([]byte, 32), transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, "revoke", 3, make([]byte, 16), nil)
			if (err == nil) != test.accepted {
				t.Fatal("unexpected acknowledgement", err)
			}
		})
	}
}

// An RPC error carries no information about whether the handler changed
// state. Exercise the real client/UDS/gRPC boundary on both sides of execution.
func TestTrustRPCUncertainOutcomes(t *testing.T) {
	for _, code := range []codes.Code{codes.DeadlineExceeded, codes.Unavailable, codes.Canceled} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", code, after), func(t *testing.T) {
				var effects atomic.Int32
				fail := func(context.Context) error {
					if after {
						effects.Add(1)
					}
					return status.Error(code, "injected lost response")
				}
				server := &trustRPCServer{update: func(ctx context.Context) (*transportv1.UpdateNodeTrustResponse, error) { return nil, fail(ctx) }, closeNode: fail}
				client, err := New(startFetchServer(t, server), time.Second, 1, uint32(os.Geteuid()), uint32(os.Getegid()))
				if err != nil {
					t.Fatal(err)
				}
				updateErr := client.UpdateNodeTrust(context.Background(), make([]byte, 16), make([]byte, 32), transportv1.NodeTrustState_NODE_TRUST_STATE_REVOKED, "revoke", 3, make([]byte, 16), nil)
				closeErr := client.CloseNode(context.Background(), make([]byte, 16), "revoke", nil)
				wantEffects := int32(0)
				if after {
					wantEffects = 2
				}
				if status.Code(updateErr) != code || status.Code(closeErr) != code || effects.Load() != wantEffects {
					t.Fatal(updateErr, closeErr, effects.Load())
				}
			})
		}
	}
}

func TestCloseNotFoundIsAbsenceWithoutExecutionReceipt(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous-close-executed=%t", removed), func(t *testing.T) {
			// Both histories expose the same current absence. The client treats
			// NOT_FOUND as satisfied absence, not a receipt for the earlier call.
			var connected atomic.Bool
			var effects atomic.Int32
			connected.Store(removed)
			server := &trustRPCServer{closeNode: func(context.Context) error {
				if connected.Swap(false) {
					effects.Add(1)
					return status.Error(codes.Unavailable, "close executed; acknowledgement lost")
				}
				return status.Error(codes.NotFound, "node is not connected")
			}}
			client, err := New(startFetchServer(t, server), time.Second, 1, uint32(os.Geteuid()), uint32(os.Getegid()))
			if err != nil {
				t.Fatal(err)
			}
			if removed {
				if err := client.CloseNode(context.Background(), make([]byte, 16), "revoke", nil); status.Code(err) != codes.Unavailable {
					t.Fatal(err)
				}
			}
			if err := client.CloseNode(context.Background(), make([]byte, 16), "revoke", nil); err != nil {
				t.Fatal(err)
			}
			wantEffects := int32(0)
			if removed {
				wantEffects = 1
			}
			if effects.Load() != wantEffects {
				t.Fatal("unexpected close history", effects.Load())
			}
		})
	}
}
