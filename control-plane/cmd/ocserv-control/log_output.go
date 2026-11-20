package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type logOutput struct {
	skipped atomic.Uint64
	mu      sync.RWMutex
	records chan []byte
	done    chan struct{}
	err     error // published by closing done
}

func newLogOutput(dst, fallback io.Writer) *logOutput {
	out := &logOutput{records: make(chan []byte, 256), done: make(chan struct{})}
	records := out.records
	go func() {
		defer close(out.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var batch bytes.Buffer
		summary := slog.New(slog.NewJSONHandler(&batch, nil))
		flush := func() {
			if n := out.skipped.Swap(0); n != 0 {
				summary.Warn("http access logs skipped", "skipped_count", n)
			}
			if batch.Len() == 0 {
				return
			}
			if _, err := batch.WriteTo(dst); err != nil {
				// WriteTo leaves just the unwritten suffix in the buffer.
				if _, fallbackErr := batch.WriteTo(fallback); fallbackErr != nil && out.err == nil {
					out.err = errors.Join(err, fallbackErr)
				}
				batch.Reset()
			}
		}
		for {
			select {
			case record, ok := <-records:
				if !ok {
					flush()
					return
				}
				batch.Write(record)
				if batch.Len() >= 32<<10 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()
	return out
}

func (out *logOutput) Write(record []byte) (int, error) {
	out.mu.RLock()
	defer out.mu.RUnlock()
	if out.records == nil {
		return 0, io.ErrClosedPipe
	}
	// JSONHandler reuses the encoded record after Write returns.
	out.records <- bytes.Clone(record)
	return len(record), nil
}

// Separate JSON handlers keep a blocked critical write from holding the
// access logger's handler lock.
type accessOutput struct{ *logOutput }

func (out accessOutput) Write(record []byte) (int, error) {
	out.mu.RLock()
	defer out.mu.RUnlock()
	if out.records == nil {
		return 0, io.ErrClosedPipe
	}
	select {
	case out.records <- bytes.Clone(record):
	default:
		out.skipped.Add(1)
	}
	return len(record), nil
}

func (out *logOutput) Close() error {
	out.mu.Lock()
	close(out.records)
	out.records = nil
	out.mu.Unlock()
	<-out.done
	return out.err
}
