//go:build !faultinject

package operations

import "context"

const faultInjection = false

type workerFaults struct{}

func (workerFaults) beforeSend(context.Context, Dispatch) error { return nil }
func (workerFaults) afterSend(context.Context, Dispatch) error  { return nil }
