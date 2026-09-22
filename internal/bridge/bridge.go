// Package bridge is the daemon: heartbeat → poll → artifact → raster → print →
// ack, every 3 seconds, forever (master-plan tasks 23 and 24).
//
// The rules that are not negotiable, and why:
//
//   - transport.Send reports BYTES WRITTEN. Any byte that reached the printer
//     makes the failure TERMINAL (`failed{partial}`, retryable false). A retry
//     after a partial write is how a venue gets two legal invoices for one
//     order (counsel finding 4). Only a zero-byte failure is retryable.
//   - An artifact whose width is not the printer's widthDots acks
//     `failed{render_failed}` and prints NOTHING. It is never resampled: a
//     silently rescaled receipt is a quality regression discovered on paper,
//     weeks later (escpos.Options.NoResample makes the resample branch
//     unreachable from here).
//   - A welcome slip is sent only to a printer the server named, or — when it
//     named none — to the single candidate, or to the one candidate that
//     answers `GS I`. Never "the first printer found": port 9100 is JetDirect
//     and the first answer can be an office LaserJet (counsel finding 6).
//   - Every job is acked within 60 s or acked `failed{timeout}`. The server's
//     lease reclaim runs at 2 minutes, so a silent daemon is not a stuck job.
//   - A 401 stops the POLLING, and never the process. A revoked device that
//     keeps polling is noise in the rate limiter; a revoked device that EXITS
//     is worse — systemd's Restart=always turns it into 260 restarts and 560
//     rejected heartbeats overnight until the server's login limiter locks the
//     box out (issue #9). So: one loud line, then a 1 → 2 → 5 → 10 minute
//     backoff on the heartbeat alone, forever, re-reading bridge.json each
//     time so an operator's re-enrolment is picked up without a restart.
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // artifact decoders
	_ "image/png"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/discover"
	"github.com/xyz/orderly-print-bridge/internal/escpos"
	"github.com/xyz/orderly-print-bridge/internal/transport"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

// Defaults from memo 5 and master-plan task 23.
const (
	DefaultPollInterval      = 3 * time.Second
	DefaultHeartbeatInterval = 30 * time.Second // the server throttles at 10 s
	DefaultAckTimeout        = 60 * time.Second
)

// SetupSweepInterval is how often a box with NO printer assigned sweeps during
// its first SetupWindow. An installer standing at the counter plugs a printer
// in and must see it on the manager page within a minute — ten is long enough
// that they leave, or start unplugging things.
const SetupSweepInterval = 60 * time.Second

// SetupWindow is how long that accelerated sweep lasts, measured from the
// daemon's start. After it, a box with no printer is a box waiting on a human,
// not on a sweep, and the normal 10-minute gap is the polite one.
const SetupWindow = 15 * time.Minute

// RevokedBackoff is the heartbeat retry schedule after Orderly rejects the
// device token: 1 → 2 → 5 → 10 minutes, then 10 for as long as it takes. The
// last step repeats; the daemon never gives up and never exits, because the
// fix (a Reset + re-enrol) happens on a human's clock.
var RevokedBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// RevokedBackoffJitter spreads each step by ±10 %, so a venue whose whole
// fleet was revoked at once does not retry in lockstep.
const RevokedBackoffJitter = 0.10

// ErrRevoked means Orderly rejected the device token. It no longer ends Run —
// see waitOutRevocation — but the poll cycle still reports it so a caller
// (and `--once`) can tell an auth failure from a transient one.
var ErrRevoked = errors.New("bridge: device token rejected — this device has been revoked in Orderly")

// Sender delivers bytes to a printer and reports how many arrived.
type Sender func(target string, data []byte) (int, error)

// Prober asks a printer target a short question (the `GS I` identity query).
type Prober func(target string, cmd []byte) ([]byte, error)

// Sweeper lists the printer candidates this box can see. Its result is
// REPORTED, never routed to: the daemon prints only to what the server assigned
// (master-plan task 49).
type Sweeper func(ctx context.Context) ([]api.DiscoveredPrinter, error)

// Bridge is one enrolled device's print loop.
type Bridge struct {
	Client *api.Client
	Cfg    *config.Config

	Send  Sender
	Probe Prober
	// Discover runs the LAN + USB sweep. nil disables discovery entirely.
	Discover Sweeper

	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	AckTimeout        time.Duration
	// SweepInterval is the MINIMUM gap between sweeps (discover.SweepInterval).
	SweepInterval time.Duration

	Logf func(format string, args ...any)
	Now  func() time.Time
	// Wait blocks for d or until ctx ends. Injectable so the revoked-token
	// backoff is provable in a unit test without 18 minutes of real time.
	Wait func(ctx context.Context, d time.Duration) error

	lastHeartbeat time.Time
	hostname      string

	// mu guards the sweep's state: the sweep runs on its own goroutine so a
	// 10-second probe of a venue's /24 can never delay a receipt.
	mu         sync.Mutex
	discovered []api.DiscoveredPrinter
	lastSweep  time.Time
	sweeping   bool
	// started/startedAt mark the daemon's first cycle; printersAssigned
	// mirrors len(Cfg.Printers). All three are read by the sweep goroutine
	// (SIGUSR1) and so live under mu rather than being read off Cfg there.
	started          bool
	startedAt        time.Time
	printersAssigned int
}

// New builds a Bridge for an enrolled config with production defaults.
func New(cfg *config.Config) *Bridge {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	return &Bridge{
		Client:            api.New(cfg.ServerURL, cfg.Token),
		Cfg:               cfg,
		Send:              transport.Send,
		Probe:             transport.Query,
		Discover:          DefaultSweep,
		PollInterval:      DefaultPollInterval,
		HeartbeatInterval: DefaultHeartbeatInterval,
		AckTimeout:        DefaultAckTimeout,
		SweepInterval:     discover.SweepInterval,
		Now:               time.Now,
		hostname:          host,
	}
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bridge) defaults() {
	if b.Send == nil {
		b.Send = transport.Send
	}
	if b.Probe == nil {
		b.Probe = transport.Query
	}
	if b.PollInterval <= 0 {
		b.PollInterval = DefaultPollInterval
	}
	if b.HeartbeatInterval <= 0 {
		b.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if b.AckTimeout <= 0 {
		b.AckTimeout = DefaultAckTimeout
	}
	if b.SweepInterval <= 0 {
		b.SweepInterval = discover.SweepInterval
	}
	if b.Wait == nil {
		b.Wait = sleepCtx
	}
	b.mu.Lock()
	if !b.started {
		b.started = true
		b.startedAt = b.now()
		b.printersAssigned = len(b.Cfg.Printers)
	}
	b.mu.Unlock()
}

// sleepCtx is the production Wait: a timer that a cancelled context beats.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// DefaultSweep is the production sweeper: this box's own /24 on TCP 9100 plus
// its Linux USB printer nodes.
func DefaultSweep(ctx context.Context) ([]api.DiscoveredPrinter, error) {
	return discover.Sweep(ctx, discover.Options{})
}

// SweepNow runs a sweep immediately, ignoring SweepInterval. It is what SIGUSR1
// is wired to: an installer standing at the counter must not have to wait out
// the interval to see the printer they just plugged in.
func (b *Bridge) SweepNow(ctx context.Context) {
	b.defaults()
	b.startSweep(ctx, true)
}

// sweepIfDue starts a background sweep when one is due. It never blocks the
// caller and never runs two at once.
func (b *Bridge) sweepIfDue(ctx context.Context) { b.startSweep(ctx, false) }

// sweepIntervalLocked is SETUP MODE: a box with no printer assigned, inside its
// first SetupWindow, sweeps every SetupSweepInterval instead of every ten
// minutes. Both conditions matter — a box that already prints must not scan a
// venue's /24 once a minute, and a box that has waited 15 minutes for an
// assignment is waiting on a human.
//
// It never SLOWS a caller-set interval (a test's 10 ms stays 10 ms): the setup
// value is a ceiling, not an override. Call with mu held.
func (b *Bridge) sweepIntervalLocked() time.Duration {
	interval := b.SweepInterval
	if interval <= 0 {
		interval = discover.SweepInterval
	}
	inSetup := b.printersAssigned == 0 && !b.startedAt.IsZero() &&
		b.now().Sub(b.startedAt) < SetupWindow
	if inSetup && interval > SetupSweepInterval {
		return SetupSweepInterval
	}
	return interval
}

func (b *Bridge) startSweep(ctx context.Context, force bool) {
	if b.Discover == nil {
		return
	}
	b.mu.Lock()
	due := force || b.lastSweep.IsZero() || b.now().Sub(b.lastSweep) >= b.sweepIntervalLocked()
	if !due || b.sweeping {
		b.mu.Unlock()
		return
	}
	b.sweeping = true
	b.lastSweep = b.now()
	b.mu.Unlock()

	// Detached: the sweep must outlive one poll tick, and a cancelled parent
	// mid-sweep would leave `sweeping` stuck true.
	go func() {
		defer func() {
			b.mu.Lock()
			b.sweeping = false
			b.mu.Unlock()
		}()
		found, err := b.Discover(context.WithoutCancel(ctx))
		if err != nil {
			b.logf("printer discovery failed (nothing else is affected): %v", err)
			return
		}
		if len(found) > api.MaxDiscovered {
			found = found[:api.MaxDiscovered]
		}
		b.mu.Lock()
		b.discovered = found
		b.mu.Unlock()
		b.logf("printer discovery: %d candidate(s) visible from this box", len(found))
	}()
}

// Discovered returns the last sweep's candidates. They are display data for the
// manager page and nothing else — never an input to selectPrinter.
func (b *Bridge) Discovered() []api.DiscoveredPrinter {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.discovered) == 0 {
		return nil
	}
	out := make([]api.DiscoveredPrinter, len(b.discovered))
	copy(out, b.discovered)
	return out
}

// Run polls until the context is cancelled. It returns for a signal or a
// genuine fault — NEVER for a rejected token, which backs off in place
// (waitOutRevocation) rather than exiting into a systemd restart loop.
func (b *Bridge) Run(ctx context.Context) error {
	b.defaults()
	b.logf("bridge %s starting: device %s, venue %s, %d printer(s) assigned, polling every %s",
		version.Version, b.Cfg.DeviceID, b.Cfg.VenueID, len(b.Cfg.Printers), b.PollInterval)

	ticker := time.NewTicker(b.PollInterval)
	defer ticker.Stop()
	for {
		if err := b.Tick(ctx); err != nil {
			if errors.Is(err, ErrRevoked) {
				if waitErr := b.waitOutRevocation(ctx, err); waitErr != nil {
					return waitErr
				}
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Everything else is transient — a venue's uplink, a deploy, a
			// printer that is off. Keep polling; the queue is the point.
			b.logf("poll cycle failed (will retry): %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitOutRevocation is what a rejected token does INSTEAD of exiting.
//
// Issue #9: the old code returned ErrRevoked, `serve` exited 1, systemd
// restarted it 5 s later, and one mis-pointed box produced 260 restarts / 560
// rejected heartbeats in three hours — until production's login limiter
// answered 429 and the box stayed locked out after the real fault was fixed.
//
// So the process stays up and keeps its place: polling stops (a revoked device
// has no jobs to claim), ONE line says what is wrong and how to fix it, and
// the heartbeat — exactly one request per step — retries on RevokedBackoff.
// bridge.json is re-read before each attempt, because the fix is another
// process (`orderly-print-bridge enroll`) writing that file; picking it up
// here is what makes a re-enrolment take effect without a restart.
//
// It returns nil when a heartbeat succeeds (resume polling), and only ever
// errors with the context's error.
func (b *Bridge) waitOutRevocation(ctx context.Context, cause error) error {
	b.logf("POLLING STOPPED — %v. This device's token is not valid on %s: it was reset or revoked in Orderly, "+
		"or this box is pointed at the wrong Orderly. Nothing will print until that is fixed. "+
		"Fix: in Orderly (Admin › Boxes) reset the box to issue a new setup code, then on the box run "+
		"`orderly-print-bridge enroll --server %s --code <new code>`. "+
		"This daemon stays up, retries the heartbeat after 1, 2, 5 then every 10 minutes, and re-reads %s "+
		"before each try — a new enrolment resumes printing with no restart.",
		cause, b.Cfg.ServerURL, b.Cfg.ServerURL, b.Cfg.Path())

	for attempt := 0; ; attempt++ {
		if err := b.Wait(ctx, jitter(backoffStep(attempt))); err != nil {
			return err
		}
		b.reloadConfig()
		// Force the one attempt this step is allowed: heartbeatIfDue would
		// otherwise skip it inside HeartbeatInterval.
		b.lastHeartbeat = time.Time{}
		err := b.heartbeatIfDue(ctx)
		switch {
		case err == nil:
			b.logf("device token accepted by %s again — polling resumed", b.Cfg.ServerURL)
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrRevoked):
			// Still rejected. Deliberately silent: the line above already said
			// everything true, and repeating it hourly is the noise this fix
			// exists to remove.
		default:
			// A different failure (uplink, deploy) on the same schedule — say
			// it, because it changes what the operator should look at.
			b.logf("still not printing: heartbeat to %s failed: %v", b.Cfg.ServerURL, err)
		}
	}
}

// backoffStep is RevokedBackoff with its last step repeating forever.
func backoffStep(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(RevokedBackoff) {
		attempt = len(RevokedBackoff) - 1
	}
	return RevokedBackoff[attempt]
}

// jitter spreads d by ±RevokedBackoffJitter so a fleet revoked at once does
// not come back in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := (rand.Float64()*2 - 1) * RevokedBackoffJitter
	return time.Duration(float64(d) * (1 + spread))
}

// reloadConfig re-reads the config file and adopts it when the identity on
// disk differs from the one in memory — `orderly-print-bridge enroll` is a
// SEPARATE process writing bridge.json, and a running daemon that never looks
// again is a daemon that needs a restart to recover. LoadFrom is a stat + a
// ~400-byte read + one Unmarshal, so once per backoff step costs nothing.
//
// Only ever called from the Run goroutine.
func (b *Bridge) reloadConfig() bool {
	path := b.Cfg.Path()
	if path == "" {
		return false
	}
	fresh, err := config.LoadFrom(path)
	if err != nil || !fresh.Enrolled() {
		return false
	}
	if fresh.Token.Reveal() == b.Cfg.Token.Reveal() && fresh.ServerURL == b.Cfg.ServerURL {
		return false
	}
	b.logf("a new enrolment is on disk at %s (device %s, venue %s, server %s); adopting it",
		path, fresh.DeviceID, fresh.VenueID, fresh.ServerURL)
	b.Cfg = fresh
	b.Client = api.New(fresh.ServerURL, fresh.Token)
	b.lastHeartbeat = time.Time{}
	b.mu.Lock()
	b.printersAssigned = len(fresh.Printers)
	// A fresh enrolment is a fresh setup: the operator who just re-enrolled
	// is standing at the counter, so the 60 s setup sweep re-opens for them.
	b.startedAt = b.now()
	b.mu.Unlock()
	return true
}

// Tick runs one heartbeat-if-due + poll + (maybe) one job. Exported so tests
// can drive the loop deterministically instead of sleeping.
func (b *Bridge) Tick(ctx context.Context) error {
	b.defaults()
	// Started before the heartbeat so the first heartbeat after a sweep carries
	// its result; it returns immediately either way.
	b.sweepIfDue(ctx)
	if err := b.heartbeatIfDue(ctx); err != nil {
		return err
	}
	resp, err := b.Client.Poll(ctx)
	if err != nil {
		return b.classify(err)
	}
	if resp.Job == nil {
		return nil
	}
	return b.handle(ctx, resp.Job)
}

// classify turns an auth rejection into ErrRevoked, which parks the loop.
//
// 429 is in the list on purpose: on the bearer path Orderly answers
// too_many_failed_authentications from a per-IP window (20 in 15 min) after
// repeated 401s — the poll endpoint has no limiter and the heartbeat throttle
// is a SQL predicate, so a 429 here can only mean "locked out for rejecting".
// Treating it as transient kept a locked-out box hammering every 3 s for the
// whole window and never re-reading bridge.json (v1.2.0 review).
func (b *Bridge) classify(err error) error {
	if api.IsStatus(err, http.StatusUnauthorized) || api.IsStatus(err, http.StatusForbidden) || api.IsStatus(err, http.StatusTooManyRequests) {
		return fmt.Errorf("%w (%v)", ErrRevoked, err)
	}
	return err
}

// PrintersDiscovered is what the heartbeat reports. In v1 the printer address
// is configured on the server and there is no network sweep (LD-18 moved
// discovery to Phase 4), so this is the count of printers Orderly has assigned
// to this device. Zero is a normal state — it is precisely what lets the
// manager page say "no printer yet" instead of leaving the box silent
// (master-plan task 24).
func (b *Bridge) PrintersDiscovered() int { return len(b.Cfg.Printers) }

func (b *Bridge) heartbeatIfDue(ctx context.Context) error {
	if !b.lastHeartbeat.IsZero() && b.now().Sub(b.lastHeartbeat) < b.HeartbeatInterval {
		return nil
	}
	resp, err := b.Client.Heartbeat(ctx, api.HeartbeatRequest{
		Version:            version.Version,
		OS:                 runtime.GOOS,
		Arch:               runtime.GOARCH,
		Hostname:           b.hostname,
		PrintersDiscovered: b.PrintersDiscovered(),
		Discovered:         b.Discovered(),
	})
	if err != nil {
		return b.classify(err)
	}
	b.lastHeartbeat = b.now()
	b.applyPrinters(resp.Printers)
	return nil
}

// applyPrinters persists a changed printer assignment so a restart with no
// network still knows where to print the backlog.
func (b *Bridge) applyPrinters(printers []api.Printer) {
	if samePrinters(b.Cfg.Printers, printers) {
		return
	}
	b.Cfg.Printers = printers
	b.mu.Lock()
	b.printersAssigned = len(printers)
	b.mu.Unlock()
	if err := b.Cfg.Save(); err != nil {
		b.logf("could not persist the printer assignment: %v", err)
		return
	}
	b.logf("printer assignment updated: %d printer(s)", len(printers))
}

func samePrinters(a, bb []api.Printer) bool {
	if len(a) != len(bb) {
		return false
	}
	for i := range a {
		if a[i] != bb[i] {
			return false
		}
	}
	return true
}

// jobOutcome is what one attempt produced.
type jobOutcome struct {
	printed bool
	// voided means the server already terminated the job (artifact 410): ack
	// `voided`, which artifact.get.ts names as the expected daemon answer.
	voided    bool
	bytes     int
	reason    string
	detail    string
	retryable bool
	// noAck means: say nothing and let the server re-queue or reclaim. Used
	// when the lease was lost (409) or the artifact could not be fetched at
	// all — acking a job we do not hold is noise at best.
	noAck bool
	err   error
}

func (b *Bridge) handle(ctx context.Context, job *api.Job) error {
	printer, reason := b.selectPrinter(job)
	if printer == nil {
		b.logf("job %s (%s): %s — nothing printed", job.ID, job.Kind, reason)
		return b.ack(ctx, job, api.Failed("", api.FailNoPrinter, reason, 0, false))
	}

	// One 60 s budget for artifact + raster + write, so a wedged printer can
	// never hold a claim past the server's lease.
	jobCtx, cancel := context.WithTimeout(ctx, b.AckTimeout)
	defer cancel()

	done := make(chan jobOutcome, 1)
	go func() { done <- b.execute(jobCtx, job, *printer) }()

	select {
	case out := <-done:
		return b.report(ctx, job, *printer, out)
	case <-jobCtx.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Bytes written is unknown, so this is terminal, not retryable —
		// the honest reading of "we do not know whether paper moved".
		b.logf("job %s (%s): did not finish within %s; acking failed{timeout}", job.ID, job.Kind, b.AckTimeout)
		return b.ack(ctx, job, api.Failed(printer.ID, api.FailTimeout,
			fmt.Sprintf("no result within the %s ack window; bytes written unknown", b.AckTimeout), 0, false))
	}
}

func (b *Bridge) execute(ctx context.Context, job *api.Job, printer api.Printer) jobOutcome {
	artifact, err := b.Client.Artifact(ctx, job.ID)
	if err != nil {
		switch {
		case api.IsStatus(err, http.StatusGone):
			// Voided (the cashier printed in the browser) or expired. Already
			// terminal server-side; ack `voided` and print nothing.
			return jobOutcome{voided: true, detail: "artifact_gone: the job was voided or expired before it printed", err: err}
		case api.IsStatus(err, http.StatusConflict):
			// lease_lost: the server reclaimed this lease and RE-QUEUED the
			// job. Abandon it silently — acking would fight the re-queue, and
			// the next poll picks it up properly.
			return jobOutcome{noAck: true, err: err}
		case api.IsStatus(err, http.StatusNotImplemented):
			// kind_not_renderable_yet — the S5 slip-route seam. Terminal:
			// waiting will not make the server grow a renderer.
			return jobOutcome{reason: api.FailRenderFailed,
				detail: "the server cannot render this job kind yet (kind_not_renderable_yet)", err: err}
		case api.IsStatus(err, http.StatusBadGateway):
			// The render app failed. Zero bytes reached paper, so a retry is
			// both safe and likely to work.
			return jobOutcome{reason: api.FailRenderFailed, retryable: true,
				detail: "the render service could not produce the receipt", err: err}
		case api.IsStatus(err, http.StatusUnauthorized):
			return jobOutcome{noAck: true, err: b.classify(err)}
		case api.IsStatus(err, http.StatusForbidden):
			// job_not_owned — not ours, or gone. The ack would 403 too.
			return jobOutcome{noAck: true, err: err}
		default:
			// A transient fetch failure: stay silent and let the lease reclaim it.
			return jobOutcome{noAck: true, err: err}
		}
	}

	img, _, err := image.Decode(bytes.NewReader(artifact))
	if err != nil {
		return jobOutcome{reason: api.FailRenderFailed,
			detail: "the artifact was not a decodable image",
			err:    fmt.Errorf("artifact is not a decodable image: %w", err)}
	}

	// The width gate. This is checked BEFORE encoding as well as inside
	// escpos (NoResample) so the failure is named precisely and no bytes are
	// ever built for the wrong head.
	got := img.Bounds().Dx()
	if printer.WidthDots <= 0 {
		return jobOutcome{reason: api.FailRenderFailed,
			detail: "this printer has no paper width configured",
			err:    fmt.Errorf("printer %s has no paper width configured", printer.Name)}
	}
	if got != printer.WidthDots {
		detail := fmt.Sprintf("artifact is %d dots wide but this printer prints %d; refusing to resample", got, printer.WidthDots)
		return jobOutcome{reason: api.FailRenderFailed, detail: detail,
			err: fmt.Errorf("artifact is %d dots wide but %s prints %d dots; refusing to resample",
				got, printer.Name, printer.WidthDots)}
	}

	opts := escpos.Options{
		Width:          printer.WidthDots,
		Threshold:      thresholdOrDefault(printer.Threshold),
		BandHeight:     bandOrDefault(printer.BandHeight),
		NoResample:     true,
		LeftMarginDots: printer.LeftMarginDots,
		// Empty (every server before this field existed) means a full cut,
		// which is what the bench proved this head honours.
		CutMode: printer.CutMode,
	}
	if printer.LeftMarginDots > 0 {
		// Say it on every print: it is a per-printer server setting a venue can
		// change without telling anyone, and a raster wider than the head is a
		// paper symptom whose cause is otherwise invisible.
		b.logf("job %s: left margin %d dots on %s (raster %d dots wide)",
			job.ID, printer.LeftMarginDots, printer.Name, printer.WidthDots+printer.LeftMarginDots)
	}
	stream, err := escpos.Encode(img, opts)
	if err != nil {
		return jobOutcome{reason: api.FailRenderFailed, detail: err.Error(), err: err}
	}

	n, err := b.Send(printer.Target(), stream)
	if err != nil {
		if n > 0 {
			// TERMINAL. Paper moved; a retry prints a second copy. The server
			// re-derives this from bytesWritten too (ack.ts rule 2).
			return jobOutcome{bytes: n, reason: api.FailPartial, retryable: false,
				detail: truncate(err.Error()), err: err}
		}
		return jobOutcome{bytes: 0, reason: api.FailOffline, retryable: true,
			detail: truncate(err.Error()), err: err}
	}
	return jobOutcome{printed: true, bytes: n}
}

func (b *Bridge) report(ctx context.Context, job *api.Job, printer api.Printer, out jobOutcome) error {
	switch {
	case out.noAck:
		b.logf("job %s (%s): %v — leaving it for the server to reclaim", job.ID, job.Kind, out.err)
		return out.err
	case out.printed:
		b.logf("job %s (%s): printed %d bytes on %s", job.ID, job.Kind, out.bytes, printer.Name)
		return b.ack(ctx, job, api.Printed(printer.ID, out.bytes))
	case out.voided:
		b.logf("job %s (%s): %v — acking voided, nothing printed", job.ID, job.Kind, out.err)
		return b.ack(ctx, job, api.Voided(printer.ID, out.detail))
	default:
		b.logf("job %s (%s): failed{%s} after %d byte(s) on %s: %v",
			job.ID, job.Kind, out.reason, out.bytes, printer.Name, out.err)
		return b.ack(ctx, job, api.Failed(printer.ID, out.reason, out.detail, out.bytes, out.retryable))
	}
}

// ack always runs on a fresh, short context: the job context may already be
// dead, and an unacked job is a job the venue watches expire.
func (b *Bridge) ack(ctx context.Context, job *api.Job, body api.AckRequest) error {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := b.Client.Ack(ackCtx, job.ID, body); err != nil {
		return b.classify(err)
	}
	return nil
}

// selectPrinter decides where a job prints, and is where counsel finding 6
// lives. A server-named printer is a deliberate assignment and is obeyed.
//
// As merged, `poll.ts` claims a job ONLY when the role's printer belongs to
// this device, so `job.printer` is non-null on every job the server hands out
// and the branch below is defence rather than a normal path: it survives so a
// future welcome/test slip issued before any role exists still cannot be
// printed blind.
func (b *Bridge) selectPrinter(job *api.Job) (*api.Printer, string) {
	if job.Printer != nil && job.Printer.Address != "" {
		p := *job.Printer
		if p.WidthDots == 0 && job.Dots > 0 {
			p.WidthDots = job.Dots
		}
		return &p, ""
	}
	if job.Kind != api.KindWelcome && job.Kind != api.KindTest {
		return nil, "the server did not name a printer for this job"
	}
	return b.WelcomeTarget()
}

// WelcomeTarget applies the never-blind rule to the assigned printers.
func (b *Bridge) WelcomeTarget() (*api.Printer, string) {
	b.defaults()
	return selectWelcomeTarget(b.Cfg.Printers, b.Probe, b.logf)
}

func selectWelcomeTarget(candidates []api.Printer, probe Prober, logf func(string, ...any)) (*api.Printer, string) {
	switch len(candidates) {
	case 0:
		return nil, "no printer is configured for this device yet"
	case 1:
		p := candidates[0]
		return &p, ""
	}

	// More than one candidate: only a printer that ANSWERS `GS I` may receive
	// an unsolicited slip. Whether a given model answers is a per-model fact —
	// the Bixolon SRP-E300's behaviour is UNKNOWN as of this build and must be
	// observed on a bench, never assumed.
	var identified []api.Printer
	for _, c := range candidates {
		reply, err := probe(c.Target(), escpos.CmdIdentity)
		if err != nil {
			if !errors.Is(err, transport.ErrQueryUnsupported) {
				logf("identity query to %s failed: %v", c.Name, err)
			}
			continue
		}
		if len(reply) == 0 {
			continue
		}
		identified = append(identified, c)
	}
	if len(identified) == 1 {
		p := identified[0]
		return &p, ""
	}
	if len(identified) == 0 {
		return nil, fmt.Sprintf("%d printers are configured and none identified itself as an ESC/POS printer; "+
			"not sending a slip blind", len(candidates))
	}
	return nil, fmt.Sprintf("%d printers answered the identity query; "+
		"assign the receipt role in Orderly to say which one", len(identified))
}

// truncate keeps a driver string inside the server's 300-char `lastError`
// budget so nothing is silently cut mid-word on the manager surface.
func truncate(s string) string {
	const max = 280
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func thresholdOrDefault(v int) uint8 {
	if v <= 0 || v > 255 {
		return 128
	}
	return uint8(v)
}

func bandOrDefault(v int) int {
	if v <= 0 {
		return 128
	}
	return v
}
