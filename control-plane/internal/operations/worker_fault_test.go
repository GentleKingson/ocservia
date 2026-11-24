package operations

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/control-plane/internal/commandlimit"
	"github.com/GentleKingson/ocservia/control-plane/internal/database"
	operationstore "github.com/GentleKingson/ocservia/control-plane/internal/operations/store"
	"github.com/GentleKingson/ocservia/control-plane/internal/testutil"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type faultDispatchRecord struct {
	dispatch Dispatch
	claimed  bool
	sent     bool
	failed   bool
	errMsg   string
}

type faultDispatchBackend struct {
	database.Backend
	mu      sync.Mutex
	now     time.Time
	records map[uuid.UUID]*faultDispatchRecord
	order   []uuid.UUID
}

func newFaultDispatchBackend(jobs []Dispatch) *faultDispatchBackend {
	records := make(map[uuid.UUID]*faultDispatchRecord, len(jobs))
	order := make([]uuid.UUID, 0, len(jobs))
	for _, job := range jobs {
		records[job.CommandID] = &faultDispatchRecord{dispatch: job}
		order = append(order, job.CommandID)
	}
	return &faultDispatchBackend{
		now:     time.Now().UTC().Truncate(time.Microsecond),
		records: records,
		order:   order,
	}
}

func (b *faultDispatchBackend) Begin(_ context.Context, _ database.Isolation) (database.Tx, error) {
	return &faultDispatchTx{backend: b}, nil
}

type faultDispatchTx struct {
	database.Tx
	backend *faultDispatchBackend
}

func (t *faultDispatchTx) Commit(context.Context) error   { return nil }
func (t *faultDispatchTx) Rollback(context.Context) error { return nil }
func (t *faultDispatchTx) WallTime(context.Context) (time.Time, error) {
	return t.backend.now, nil
}
func (t *faultDispatchTx) CommandLimitStore() commandlimit.Store { return &faultCommandLimitStore{} }
func (t *faultDispatchTx) OperationStore() operationstore.Store {
	return &faultOperationStore{backend: t.backend}
}

type faultCommandLimitStore struct{}

func (*faultCommandLimitStore) LockAdmission(context.Context) error      { return nil }
func (*faultCommandLimitStore) LockBacklog(context.Context) error        { return nil }
func (*faultCommandLimitStore) Active(context.Context, int) (int, error) { return 0, nil }
func (*faultCommandLimitStore) NodeBacklog(context.Context, uuid.UUID, int) (int, error) {
	return 0, nil
}
func (*faultCommandLimitStore) WorkspaceBacklog(context.Context, uuid.UUID, int) (int, error) {
	return 0, nil
}

type faultOperationStore struct {
	operationstore.Store
	backend *faultDispatchBackend
}

func (s *faultOperationStore) DispatchCandidates(_ context.Context, limit, available int, _ time.Time) ([]Dispatch, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	maxCount := min(limit, available)
	out := make([]Dispatch, 0, maxCount)
	for _, id := range s.backend.order {
		if len(out) >= maxCount {
			break
		}
		rec := s.backend.records[id]
		if !rec.claimed && !rec.sent && !rec.failed {
			out = append(out, rec.dispatch)
		}
	}
	return out, nil
}

func (s *faultOperationStore) ClaimDispatch(_ context.Context, d Dispatch, _ uuid.UUID, _, _ time.Time) (bool, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	rec := s.backend.records[d.CommandID]
	if rec == nil || rec.claimed {
		return false, nil
	}
	rec.claimed = true
	rec.dispatch = d
	return true, nil
}

func (s *faultOperationStore) LockDispatchOutbox(context.Context, Dispatch) error { return nil }

func (s *faultOperationStore) DispatchStatus(_ context.Context, d Dispatch) (operationstore.DispatchStatus, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	rec := s.backend.records[d.CommandID]
	if rec == nil || !rec.claimed {
		return operationstore.DispatchStatus{}, database.ErrNotFound
	}
	return operationstore.DispatchStatus{
		LeaseValid:     true,
		AttemptValid:   true,
		OwnsOutboxLock: true,
		CommandState:   "queued",
		Envelope:       rec.dispatch.Envelope,
	}, nil
}

func (s *faultOperationStore) PublishDispatch(context.Context, Dispatch, time.Time) error {
	return nil
}

func (s *faultOperationStore) RecordDispatched(_ context.Context, d Dispatch, _ bool, _ time.Time) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	s.backend.records[d.CommandID].sent = true
	return nil
}

func (s *faultOperationStore) CloseDispatch(context.Context, Dispatch, time.Time) error {
	return nil
}

func (s *faultOperationStore) FailDispatch(_ context.Context, d Dispatch, message string, _ time.Time) error {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	rec := s.backend.records[d.CommandID]
	rec.failed = true
	rec.errMsg = message
	return nil
}

type faultInjectingSender struct {
	stalledNode  uuid.UUID
	stalledEnter chan struct{}
	releaseStall chan struct{}
	stallOnce    sync.Once

	inFlight   atomic.Int32
	peakFlight atomic.Int32
	mu         sync.Mutex
	sentNodes  map[uuid.UUID]int
	healthyWg  sync.WaitGroup
}

func (s *faultInjectingSender) SendCommand(ctx context.Context, rawNodeID, _ []byte) error {
	nodeID, err := uuid.FromBytes(rawNodeID)
	if err != nil {
		return err
	}
	cur := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		peak := s.peakFlight.Load()
		if cur <= peak || s.peakFlight.CompareAndSwap(peak, cur) {
			break
		}
	}

	s.mu.Lock()
	s.sentNodes[nodeID]++
	s.mu.Unlock()

	if nodeID == s.stalledNode {
		s.stallOnce.Do(func() { close(s.stalledEnter) })
		select {
		case <-s.releaseStall:
			return errors.New("stalled peer timed out")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Hold healthy sends briefly so concurrent workers overlap measurably.
	time.Sleep(15 * time.Millisecond)
	s.healthyWg.Done()
	return nil
}

func TestWorkerDispatchBatchIsolatesStalledNodeAndBoundsConcurrency(t *testing.T) {
	const totalNodes = 12
	signer := testCommandSigner(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	jobs := make([]Dispatch, 0, totalNodes)
	for range totalNodes {
		nodeID := uuid.Must(uuid.NewV7())
		opID := uuid.Must(uuid.NewV7())
		cmdID := uuid.Must(uuid.NewV7())
		req := CreateRequest{
			NodeID:          nodeID,
			IdempotencyKey:  uuid.NewString(),
			ExpectedVersion: 1,
			TTL:             time.Minute,
			RequestID:       uuid.NewString(),
			Traceparent:     testTraceparent,
			Payload:         SyntheticNoopPayload{},
		}
		envelope, _, err := marshalEnvelope(req, opID, cmdID, 1, now, now.Add(time.Minute), signer)
		testutil.NoError(t, err)
		jobs = append(jobs, Dispatch{
			CommandID:   cmdID,
			OperationID: opID,
			OutboxID:    uuid.Must(uuid.NewV7()),
			NodeID:      nodeID,
			Envelope:    envelope,
			Traceparent: testTraceparent,
		})
	}

	backend := newFaultDispatchBackend(jobs)
	service := NewBackend(backend, 64, signer)
	sender := &faultInjectingSender{
		stalledNode:  jobs[0].NodeID,
		stalledEnter: make(chan struct{}),
		releaseStall: make(chan struct{}),
		sentNodes:    make(map[uuid.UUID]int, totalNodes),
	}
	sender.healthyWg.Add(totalNodes - 1)
	worker, err := NewWorker(service, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	testutil.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	type batchResult struct {
		claimed int
		full    bool
		err     error
	}
	done := make(chan batchResult, 1)
	go func() {
		claimed, full, err := worker.dispatchBatch(ctx)
		done <- batchResult{claimed: claimed, full: full, err: err}
	}()

	select {
	case <-sender.stalledEnter:
	case <-ctx.Done():
		t.Fatal("stalled node never entered SendCommand")
	}

	// All 11 healthy nodes must complete while node 0 remains stalled.
	healthyDone := make(chan struct{})
	go func() {
		sender.healthyWg.Wait()
		close(healthyDone)
	}()
	select {
	case <-healthyDone:
	case <-ctx.Done():
		t.Fatal("stalled node blocked healthy nodes in the same outbox batch")
	}

	close(sender.releaseStall)
	res := <-done
	testutil.NoError(t, res.err)
	if res.claimed != totalNodes || res.full {
		t.Fatalf("dispatchBatch() = claimed=%d full=%v, want claimed=%d full=false", res.claimed, res.full, totalNodes)
	}

	peak := int(sender.peakFlight.Load())
	if peak < 2 || peak > maxDispatchConcurrency {
		t.Fatalf("peak concurrent dispatches = %d, want 2..%d", peak, maxDispatchConcurrency)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	for i, job := range jobs {
		rec := backend.records[job.CommandID]
		if sender.sentNodes[job.NodeID] != 1 {
			t.Fatalf("node %d SendCommand calls = %d, want 1", i, sender.sentNodes[job.NodeID])
		}
		if i == 0 {
			if !rec.failed || rec.sent || rec.errMsg != "stalled peer timed out" {
				t.Fatalf("stalled job state = failed:%v sent:%v msg:%q", rec.failed, rec.sent, rec.errMsg)
			}
		} else if !rec.sent || rec.failed {
			t.Fatalf("healthy job %d state = sent:%v failed:%v", i, rec.sent, rec.failed)
		}
	}
}

type staticSender struct{ err error }

func (s staticSender) SendCommand(context.Context, []byte, []byte) error { return s.err }

func TestWorkerLeavesSendingLeaseOnAmbiguousErrorAndFailsPreSendRejection(t *testing.T) {
	signer := testCommandSigner(t)
	newJob := func() Dispatch {
		nodeID := uuid.Must(uuid.NewV7())
		opID := uuid.Must(uuid.NewV7())
		cmdID := uuid.Must(uuid.NewV7())
		return Dispatch{CommandID: cmdID, OperationID: opID, OutboxID: uuid.Must(uuid.NewV7()), NodeID: nodeID, Envelope: []byte("env"), Traceparent: testTraceparent}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name       string
		sendErr    error
		wantFailed bool
	}{
		{"pre-send disconnected", status.Error(codes.FailedPrecondition, "node is not connected"), true},
		{"ambiguous unavailable", status.Error(codes.Unavailable, "failed to write command"), false},
		{"ambiguous deadline exceeded", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := newJob()
			backend := newFaultDispatchBackend([]Dispatch{job})
			backend.records[job.CommandID].claimed = true
			service := NewBackend(backend, 64, signer)
			worker, err := NewWorker(service, staticSender{err: tc.sendErr}, logger)
			testutil.NoError(t, err)
			worker.dispatchOne(context.Background(), job)

			rec := backend.records[job.CommandID]
			if rec.failed != tc.wantFailed || rec.sent {
				t.Fatalf("job state = failed:%v sent:%v, want failed:%v", rec.failed, rec.sent, tc.wantFailed)
			}
		})
	}
}
