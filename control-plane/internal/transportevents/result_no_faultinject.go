//go:build !faultinject

package transportevents

import (
	"context"
	"time"

	transportv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/transport/v1"
)

const faultInjection = false

type resultCommitFault struct{}

func (resultCommitFault) beforeResultCommit(context.Context, *transportv1.TransportEvent, time.Time) error {
	return nil
}
