package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/update"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

const updateUsage = `orderly-print-bridge self-update — install the latest v1 release

  Reads the public release feed, refuses anything outside this binary's major
  version, downloads the binary for this OS/arch, verifies it against the
  release's SHA256SUMS, and only then renames it into place. A checksum that
  does not match aborts and keeps the running binary.

  Offline, or GitHub rate-limiting this IP, is a logged skip and exit 0 — a
  venue with no uplink must not see a failed unit.

FLAGS
  --check         report whether an update is available; install nothing
  --unit <name>   systemd unit to restart afterwards (default orderly-bridge)
  --no-restart    install the new binary but leave the running one alone
`

// DefaultServiceUnit is the systemd unit install.sh writes.
const DefaultServiceUnit = "orderly-bridge"

func runSelfUpdate(args []string) error {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, updateUsage) }
	checkOnly := fs.Bool("check", false, "report whether an update is available, install nothing")
	unit := fs.String("unit", DefaultServiceUnit, "systemd unit to restart after installing")
	noRestart := fs.Bool("no-restart", false, "do not restart the service after installing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u := &update.Updater{Logf: logger.Printf}
	rel, err := u.Check(ctx)
	if err != nil {
		if update.Skippable(err) {
			// Up to date, offline, rate-limited, or a new major line: all of
			// these are "nothing to do today", and a nightly timer that fails
			// on them is a nightly alert nobody reads.
			logger.Printf("%v", err)
			return nil
		}
		return err
	}
	if *checkOnly {
		fmt.Printf("an update is available: %s (running %s)\n", rel.TagName, version.Version)
		fmt.Println("install it with: orderly-print-bridge self-update")
		return nil
	}

	logger.Printf("installing %s over %s…", rel.TagName, version.Version)
	path, err := u.Apply(ctx, rel)
	if err != nil {
		// A checksum mismatch is LOUD: exit non-zero so the timer's unit shows
		// failed and a human looks. The running binary is untouched either way.
		return err
	}
	logger.Printf("installed %s at %s", rel.TagName, path)

	if *noRestart {
		logger.Printf("not restarting: the new binary starts on the next restart of %s", *unit)
		return nil
	}
	restartService(ctx, *unit)
	return nil
}

// restartService asks systemd to restart the bridge so the freshly installed
// binary is the one running. Best-effort by design: a PC install with no
// systemd, or a unit under another name, is not a failed update — the binary is
// already in place and the next start picks it up.
func restartService(ctx context.Context, unit string) {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		logger.Printf("no systemd here; restart the bridge to run the new binary")
		return
	}
	// --no-block: when this process IS the unit being restarted, waiting for the
	// job to finish is waiting for ourselves to die.
	out, err := exec.CommandContext(ctx, systemctl, "restart", "--no-block", unit).CombinedOutput()
	if err != nil {
		logger.Printf("could not restart %s (%v: %s); restart it to run the new binary",
			unit, err, string(out))
		return
	}
	logger.Printf("asked systemd to restart %s", unit)
}

// watchForUpdates runs the periodic check inside `serve`.
//
// The MECHANISM of record is the nightly `orderly-bridge-update.timer` that
// install.sh writes (master-plan task 47) — a one-shot process that updates and
// restarts the unit is far easier to reason about than a daemon replacing its
// own binary. This in-process check exists for the installs that have no timer:
// by default it only SAYS an update is available (visible in `journalctl`), and
// `serve --auto-update` makes it install and restart.
//
// The interval is jittered ±10%, so a street of venues that all restarted after
// the same power cut do not all hit GitHub in the same second.
func watchForUpdates(ctx context.Context, every time.Duration, auto bool, unit string) {
	if every <= 0 {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(every)):
			}
			u := &update.Updater{Logf: logger.Printf}
			rel, err := u.Check(ctx)
			if err != nil {
				if !update.Skippable(err) || errors.Is(err, update.ErrDifferentMajor) {
					logger.Printf("update check: %v", err)
				}
				continue
			}
			if !auto {
				logger.Printf("an update is available: %s (running %s). Install it with `orderly-print-bridge self-update`.",
					rel.TagName, version.Version)
				continue
			}
			path, err := u.Apply(ctx, rel)
			if err != nil {
				logger.Printf("automatic update refused: %v", err)
				continue
			}
			logger.Printf("installed %s at %s; restarting", rel.TagName, path)
			restartService(ctx, unit)
		}
	}()
}

func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 5 // ±10%
	if spread <= 0 {
		return d
	}
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread))
}
