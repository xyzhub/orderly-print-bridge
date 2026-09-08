//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/xyz/orderly-print-bridge/internal/bridge"
)

// onDemandSweep wires SIGUSR1 to an immediate discovery sweep.
//
// It exists for the installer standing at the counter: they plug the printer
// in, and `systemctl kill -s USR1 orderly-bridge` puts it on the manager page
// within one heartbeat instead of up to ten minutes later. SIGUSR1 does not
// exist on Windows, hence the split file.
func onDemandSweep(ctx context.Context, b *bridge.Bridge, logf func(string, ...any)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				logf("SIGUSR1: sweeping for printers now")
				b.SweepNow(ctx)
			}
		}
	}()
}
