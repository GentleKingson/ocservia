//go:build faultinject

package transportevents

import (
	"context"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

const faultInjection = true

type resultCommitFault struct {
	beforeCommit func(context.Context, *transportv1.TransportEvent, time.Time) error
}

func (s *Service) SetResultCommitHook(before func(context.Context, *transportv1.TransportEvent, time.Time) error) {
	s.beforeCommit = before
}

func (s resultCommitFault) beforeResultCommit(ctx context.Context, event *transportv1.TransportEvent, at time.Time) error {
	if s.beforeCommit != nil {
		return s.beforeCommit(ctx, event, at)
	}
	return nil
}
