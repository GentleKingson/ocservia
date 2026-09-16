package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type shutdownServer interface{ Shutdown(context.Context) error }

type runningComponent struct {
	done chan struct{}
	err  error // Published by closing done.
	name string
}

// lifecycle owns only running tasks and listeners, never business services.
// Assembly and cleanup run on the caller; each task publishes exactly once.
type lifecycle struct {
	ctx         context.Context
	stop        context.CancelFunc
	timeout     time.Duration
	logger      *slog.Logger
	http, trust shutdownServer
	tasks       []*runningComponent
	first       chan *runningComponent
}

func newLifecycle(ctx context.Context, timeout time.Duration, logger *slog.Logger) *lifecycle {
	ctx, stop := context.WithCancel(ctx)
	return &lifecycle{ctx: ctx, stop: stop, timeout: timeout, logger: logger, first: make(chan *runningComponent, 1)}
}

func (l *lifecycle) start(name string, run func(context.Context) error) {
	task := &runningComponent{name: name, done: make(chan struct{})}
	l.tasks = append(l.tasks, task)
	go func() {
		if err := run(l.ctx); err != nil {
			task.err = fmt.Errorf("%s: %w", name, err)
		}
		close(task.done)
		select {
		case l.first <- task:
		default:
		}
	}()
}

func (l *lifecycle) wait() error {
	select {
	case task := <-l.first:
		return task.err
	case <-l.ctx.Done():
		return l.ctx.Err()
	}
}

func (l *lifecycle) close(primary error) error {
	l.stop()
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	failures := []error{failureCause(primary)}
	// Close API admission/SSE before Trust, then join all database users.
	for _, server := range []struct {
		name   string
		server shutdownServer
	}{
		{"shutdown HTTP server", l.http}, {"shutdown trust UDS", l.trust},
	} {
		if server.server != nil {
			if err := server.server.Shutdown(ctx); err != nil {
				l.logger.Error(server.name, "error", err)
				if cause := failureCause(err); cause != nil {
					failures = append(failures, fmt.Errorf("%s: %w", server.name, cause))
				}
			}
		}
	}
	for _, task := range l.tasks {
		select {
		case <-task.done:
		default:
			select {
			case <-task.done:
			case <-ctx.Done():
				failures = append(failures, fmt.Errorf("wait for %s: %w", task.name, ctx.Err()))
				continue
			}
		}
		if task.err != primary {
			if err := failureCause(task.err); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return primary
}

// CLI treats any error matching context.Canceled as a successful shutdown.
// Remove cancellation leaves, not real failures sharing a joined error tree.
func failureCause(err error) error {
	if !errors.Is(err, context.Canceled) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures []error
		for _, cause := range joined.Unwrap() {
			failures = append(failures, failureCause(cause))
		}
		return errors.Join(failures...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if cause := failureCause(wrapped.Unwrap()); cause != nil {
			return fmt.Errorf("%v: %w", err, cause)
		}
	}
	return nil
}
