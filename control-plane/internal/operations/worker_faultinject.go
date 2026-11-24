//go:build faultinject

package operations

import "context"

const faultInjection = true

type workerFaults struct {
	before, after func(context.Context, Dispatch) error
}

func (w *Worker) SetDispatchHooks(before, after func(context.Context, Dispatch) error) {
	w.before, w.after = before, after
}

func (w workerFaults) beforeSend(ctx context.Context, job Dispatch) error {
	if w.before != nil {
		return w.before(ctx, job)
	}
	return nil
}

func (w workerFaults) afterSend(ctx context.Context, job Dispatch) error {
	if w.after != nil {
		return w.after(ctx, job)
	}
	return nil
}
