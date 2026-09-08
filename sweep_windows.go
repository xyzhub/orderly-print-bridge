//go:build windows

package main

import (
	"context"

	"github.com/xyz/orderly-print-bridge/internal/bridge"
)

// onDemandSweep is a no-op on Windows: there is no SIGUSR1. A PC install sweeps
// on its interval, and a restart of the service forces one immediately.
func onDemandSweep(_ context.Context, _ *bridge.Bridge, _ func(string, ...any)) {}
