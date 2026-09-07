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
//   - A 401 stops the loop: the device was revoked, and a revoked device that
//     keeps polling is just noise in the rate limiter.
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // artifact decoders
	_ "image/png"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/config"
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

// ErrRevoked ends Run: Orderly rejected the device token.
var ErrRevoked = errors.New("bridge: device token rejected — this device has been revoked in Orderly")

// Sender delivers bytes to a printer and reports how many arrived.
type Sender func(target string, data []byte) (int, error)

// Prober asks a printer target a short question (the `GS I` identity query).
type Prober func(target string, cmd []byte) ([]byte, error)

// Bridge is one enrolled device's print loop.
type Bridge struct {
	Client *api.Client
	Cfg    *config.Config

	Send  Sender
	Probe Prober

	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	AckTimeout        time.Duration

	Logf func(format string, args ...any)
	Now  func() time.Time

	lastHeartbeat time.Time
	hostname      string
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
		PollInterval:      DefaultPollInterval,
		HeartbeatInterval: DefaultHeartbeatInterval,
		AckTimeout:        DefaultAckTimeout,
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
}

// Run polls until the context is cancelled or the device is revoked.
func (b *Bridge) Run(ctx context.Context) error {
	b.defaults()
	b.logf("bridge %s starting: device %s, venue %s, %d printer(s) assigned, polling every %s",
		version.Version, b.Cfg.DeviceID, b.Cfg.VenueID, len(b.Cfg.Printers), b.PollInterval)

	ticker := time.NewTicker(b.PollInterval)
	defer ticker.Stop()
	for {
		if err := b.Tick(ctx); err != nil {
			if errors.Is(err, ErrRevoked) {
				b.logf("%v", err)
				return err
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

// Tick runs one heartbeat-if-due + poll + (maybe) one job. Exported so tests
// can drive the loop deterministically instead of sleeping.
func (b *Bridge) Tick(ctx context.Context) error {
	b.defaults()
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

// classify turns a 401 into the loop-stopping ErrRevoked.
func (b *Bridge) classify(err error) error {
	if api.IsStatus(err, http.StatusUnauthorized) || api.IsStatus(err, http.StatusForbidden) {
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
	printed   bool
	bytes     int
	reason    string
	retryable bool
	// noAck means: say nothing and let the server's 2-minute lease reclaim
	// re-queue the job. Used when we could not even fetch the artifact.
	noAck bool
	err   error
}

func (b *Bridge) handle(ctx context.Context, job *api.Job) error {
	printer, reason := b.selectPrinter(job)
	if printer == nil {
		b.logf("job %s (%s): %s — nothing printed", job.ID, job.Kind, reason)
		return b.ack(ctx, job, api.Failed("", api.FailNoPrinter, 0, false))
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
		return b.ack(ctx, job, api.Failed(printer.ID, api.FailTimeout, 0, false))
	}
}

func (b *Bridge) execute(ctx context.Context, job *api.Job, printer api.Printer) jobOutcome {
	artifact, err := b.Client.Artifact(ctx, job.ID)
	if err != nil {
		if api.IsStatus(err, http.StatusGone) {
			return jobOutcome{reason: api.FailArtifactGone, err: err}
		}
		if api.IsStatus(err, http.StatusUnauthorized) || api.IsStatus(err, http.StatusForbidden) {
			return jobOutcome{noAck: true, err: b.classify(err)}
		}
		// A transient fetch failure: stay silent and let the lease reclaim it.
		return jobOutcome{noAck: true, err: err}
	}

	img, _, err := image.Decode(bytes.NewReader(artifact))
	if err != nil {
		return jobOutcome{reason: api.FailRenderFailed,
			err: fmt.Errorf("artifact is not a decodable image: %w", err)}
	}

	// The width gate. This is checked BEFORE encoding as well as inside
	// escpos (NoResample) so the failure is named precisely and no bytes are
	// ever built for the wrong head.
	got := img.Bounds().Dx()
	if printer.WidthDots <= 0 {
		return jobOutcome{reason: api.FailRenderFailed,
			err: fmt.Errorf("printer %s has no paper width configured", printer.Name)}
	}
	if got != printer.WidthDots {
		return jobOutcome{reason: api.FailRenderFailed,
			err: fmt.Errorf("artifact is %d dots wide but %s prints %d dots; refusing to resample",
				got, printer.Name, printer.WidthDots)}
	}

	opts := escpos.Options{
		Width:      printer.WidthDots,
		Threshold:  thresholdOrDefault(printer.Threshold),
		BandHeight: bandOrDefault(printer.BandHeight),
		NoResample: true,
	}
	stream, err := escpos.Encode(img, opts)
	if err != nil {
		return jobOutcome{reason: api.FailRenderFailed, err: err}
	}

	n, err := b.Send(printer.Target(), stream)
	if err != nil {
		if n > 0 {
			// TERMINAL. Paper moved; a retry prints a second copy.
			return jobOutcome{bytes: n, reason: api.FailPartial, retryable: false, err: err}
		}
		return jobOutcome{bytes: 0, reason: api.FailUnreachable, retryable: true, err: err}
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
	default:
		b.logf("job %s (%s): failed{%s} after %d byte(s) on %s: %v",
			job.ID, job.Kind, out.reason, out.bytes, printer.Name, out.err)
		return b.ack(ctx, job, api.Failed(printer.ID, out.reason, out.bytes, out.retryable))
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
// lives. A server-named printer is a deliberate assignment and is obeyed. Only
// when the server named none — the welcome slip on a device with no role yet —
// does the daemon choose, and then only from an unambiguous candidate.
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
