package coordination

import "context"

type fenceContextKey struct{}

// WithFence attaches a leadership fence to the context. Only work started
// under a scheduler leadership session carries the fence, so API and worker
// paths sharing the same service instances stay unfenced.
func WithFence(ctx context.Context, fence Fence) context.Context {
	return context.WithValue(ctx, fenceContextKey{}, fence)
}

// FenceFromContext returns the fence carried by the scheduler session, or
// nil when the call is not running under scheduler leadership.
func FenceFromContext(ctx context.Context) Fence {
	fence, _ := ctx.Value(fenceContextKey{}).(Fence)
	return fence
}
