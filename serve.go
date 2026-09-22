package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/bridge"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/discover"
	"github.com/xyz/orderly-print-bridge/internal/enroll"
	"github.com/xyz/orderly-print-bridge/internal/tailnet"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

const serveUsage = `orderly-print-bridge serve — run the print daemon

  Enrolls if this device has no token yet (setup code + hardware serial), then
  polls Orderly every 3s for print jobs, rasterises them to ESC/POS and
  acknowledges each one.

FLAGS
  --server <url>       Orderly base URL (e.g. https://orderly-staging.fly.dev)
                       FIRST ENROLMENT ONLY. Once this device holds a token,
                       bridge.json's serverUrl is the truth and this flag is
                       ignored (with a log line). Default before enrolment:
                       /etc/orderly/server-url, beside the setup code.
  --config <path>      config file (default: the OS location, see docs)
  --setup-code <path>  per-flash setup code file (default /etc/orderly/setup-code)
  --code <code>        setup code, typed instead of read from the file
  --no-local-page      do not fall back to the setup page on 127.0.0.1:47831
  --poll <duration>    poll interval (default 3s)
  --once               run one poll cycle and exit (for smoke tests)
  --update-check <d>   how often to check for a new release (default 24h, 0 off)
  --auto-update        install a new release automatically, then restart
  --unit <name>        systemd unit to restart after an automatic update

SIGNALS
  SIGUSR1              sweep the LAN + USB for printers right now (POSIX only);
                       otherwise the sweep runs at most every 10 minutes
`

const enrollUsage = `orderly-print-bridge enroll — claim a setup code, store the token, exit

  This is the ONLY way to move an already-enrolled box to another Orderly: it
  replaces the stored token, server URL and ids with the ones the new setup
  code mints. Get that code from Orderly (Admin › Boxes › Issue a new setup
  code) — a Reset there is what makes the old token dead.

FLAGS
  --server <url>       Orderly base URL (required for the first enrolment;
                       otherwise /etc/orderly/server-url, then the stored one)
  --config <path>      config file (default: the OS location)
  --setup-code <path>  per-flash setup code file (default /etc/orderly/setup-code)
  --code <code>        setup code, typed instead of read from the file
  --no-local-page      do not fall back to the setup page on 127.0.0.1:47831
`

// logger writes to stderr with no timestamp prefix duplication; systemd and
// Docker add their own. The device token is a secret.Secret and redacts itself
// through every formatting verb, so no log line can carry it.
var logger = log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

type commonFlags struct {
	server      string
	configPath  string
	setupCode   string
	code        string
	noLocalPage bool
}

func bindCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.server, "server", "", "Orderly base URL")
	fs.StringVar(&c.configPath, "config", "", "config file path")
	fs.StringVar(&c.setupCode, "setup-code", config.SetupCodePathLinux, "per-flash setup code file")
	fs.StringVar(&c.code, "code", "", "setup code (instead of the file)")
	fs.BoolVar(&c.noLocalPage, "no-local-page", false, "do not fall back to the local setup page")
}

func (c *commonFlags) path() string {
	if c.configPath != "" {
		return c.configPath
	}
	return config.DefaultPath()
}

// serverURLFromFile is the PRE-ENROLMENT default for --server: the URL the
// installer wrote beside the setup code (/etc/orderly/server-url). It is read
// only when this device holds no token, so a fresh box's unit can be a bare
// `serve` — the flag that re-pointed an enrolled box is then not there to be
// inherited by the next reboot (issue #10).
//
// Every failure is "" and a log line: a hint file is a convenience, and the
// operator's own --server (or an existing enrolment) must still decide.
func serverURLFromFile(c *commonFlags) string {
	path := config.ServerURLPath(c.path())
	url, err := config.ReadServerURL(path)
	switch {
	case err == nil:
		// Deliberately does not say "not enrolled": install.sh classifies the
		// journal by substring and `*enrolled*` would read this as success.
		logger.Printf("no device token yet; using the server URL from %s: %s", path, url)
		return url
	case errors.Is(err, os.ErrNotExist):
		return ""
	default:
		logger.Printf("ignoring %s: %v", path, err)
		return ""
	}
}

func (c *commonFlags) enrollOptions(serverURL string, onTailnet func(tailnet.Join)) enroll.Options {
	return enroll.Options{
		ServerURL:      serverURL,
		ConfigPath:     c.path(),
		SetupCodePath:  c.setupCode,
		Code:           c.code,
		AllowLocalPage: !c.noLocalPage,
		Logf:           logger.Printf,
		OnTailnet:      onTailnet,
	}
}

// joinTailnet runs `tailscale up` with the key the server minted. Every failure
// is a warning: a box that cannot reach the tailnet must still print (LD-17),
// and support reachability is not worth a daemon that refuses to start.
func joinTailnet(ctx context.Context, join tailnet.Join, logf func(string, ...any)) {
	j := &tailnet.Joiner{Logf: logf}
	if err := j.Up(ctx, join); err != nil {
		if errors.Is(err, tailnet.ErrNoKey) || errors.Is(err, tailnet.ErrNotInstalled) {
			// Already said its one line, at the right volume.
			return
		}
		logf("tailnet join failed (printing is unaffected): %v", err)
	}
}

// joinTailnetInBackground is what `serve` uses: enrolment is done, the poll
// loop must start now, and `tailscale up` can take a minute on a cold uplink.
// The context is detached deliberately — a join half-run because the parent
// moved on is a worse state than one that finishes.
func joinTailnetInBackground(ctx context.Context, join tailnet.Join) {
	detached := context.WithoutCancel(ctx)
	// The log sink is bound HERE, on the caller's goroutine: a detached
	// goroutine that reads the package logger minutes later races anything
	// that replaces it.
	logf := logger.Printf
	go joinTailnet(detached, join, logf)
}

func runEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, enrollUsage) }
	var c commonFlags
	bindCommon(fs, &c)
	if err := fs.Parse(args); err != nil {
		return err
	}

	serverURL := c.server
	if serverURL == "" {
		if cfg, err := config.LoadFrom(c.path()); err == nil {
			serverURL = cfg.ServerURL
		}
	}
	if serverURL == "" {
		serverURL = serverURLFromFile(&c)
	}
	if serverURL == "" {
		fs.Usage()
		return errors.New("--server is required for the first enrollment")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The one-shot `enroll` command joins SYNCHRONOUSLY: the process is about
	// to exit, and a backgrounded join would be killed mid-flight.
	_, err := enroll.Run(ctx, c.enrollOptions(serverURL, func(join tailnet.Join) {
		joinTailnet(ctx, join, logger.Printf)
	}))
	return err
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, serveUsage) }
	var c commonFlags
	bindCommon(fs, &c)
	poll := fs.Duration("poll", bridge.DefaultPollInterval, "poll interval")
	once := fs.Bool("once", false, "run a single poll cycle and exit")
	updateEvery := fs.Duration("update-check", 24*time.Hour, "how often to check for a new release (0 disables)")
	autoUpdate := fs.Bool("auto-update", false, "install a new release automatically and restart")
	unit := fs.String("unit", DefaultServiceUnit, "systemd unit to restart after an automatic update")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadOrEnroll(ctx, &c)
	if err != nil {
		return err
	}

	b := bridge.New(cfg)
	b.Logf = logger.Printf
	if *poll > 0 {
		b.PollInterval = *poll
	}

	if *once {
		return b.Tick(ctx)
	}

	// `systemctl kill -s USR1 orderly-bridge` = sweep for printers now.
	onDemandSweep(ctx, b, logger.Printf)
	// Every ~24 h: say whether a new release exists (and with --auto-update,
	// install it). The install path of record is still the nightly timer.
	watchForUpdates(ctx, *updateEvery, *autoUpdate, *unit)

	// b.Run no longer returns ErrRevoked: a rejected token now backs off inside
	// the loop (1 → 2 → 5 → 10 min) instead of exiting into systemd's
	// `Restart=always RestartSec=5`, which turned one revoked box into 260
	// restarts and a 429 lockout overnight (issue #9). The only exits left are
	// a signal and a genuine fault.
	err = b.Run(ctx)
	if errors.Is(err, context.Canceled) {
		logger.Printf("bridge %s stopped", version.Version)
		return nil
	}
	return err
}

// loadOrEnroll returns an enrolled config, enrolling if the device has no
// token yet. A missing config on a freshly flashed box is the normal path, not
// an error.
//
// ONCE ENROLLED, bridge.json IS THE TRUTH. A `--server` that disagrees is
// reported and ignored, never written: the flag lives in a systemd unit that
// outlives the install which wrote it, and on 2026-09-21 that rewrite pointed
// a staging box's token at production on a routine reboot — 401, restart loop,
// rate-limit lockout, with nothing wrong on either server (issue #10). Moving
// a box between Orderlys is a deliberate re-enrolment, never a flag.
func loadOrEnroll(ctx context.Context, c *commonFlags) (*config.Config, error) {
	cfg, err := config.LoadFrom(c.path())
	switch {
	case err == nil && cfg.Enrolled():
		if c.server != "" && strings.TrimRight(c.server, "/") != strings.TrimRight(cfg.ServerURL, "/") {
			// Deliberately avoids the word install.sh greps for as success
			// (`*enrolled*`); this is a warning, not an enrolment.
			logger.Printf("IGNORING --server %s: this device already holds a device token for %s, "+
				"and a box that holds one never changes server because of a flag. "+
				"To move it, issue a new setup code in Orderly (Admin › Boxes › Reset) and run: "+
				"orderly-print-bridge enroll --server %s --code <new code>",
				c.server, cfg.ServerURL, c.server)
		}
		return cfg, nil
	case err != nil && !errors.Is(err, config.ErrNotFound):
		return nil, err
	}

	serverURL := c.server
	if cfg != nil && serverURL == "" {
		serverURL = cfg.ServerURL
	}
	if serverURL == "" {
		serverURL = serverURLFromFile(c)
	}
	if serverURL == "" {
		return nil, fmt.Errorf("this device is not enrolled and no --server was given (and no server URL at %s)",
			config.ServerURLPath(c.path()))
	}

	// Enrollment must not depend on Tailscale or anything else being up
	// (LD-17), but it does depend on Orderly being reachable — so retry
	// rather than exiting: a box powered on before its uplink is a normal
	// first boot, and a dead container is a box that never prints.
	backoff := 5 * time.Second
	for attempt := 1; ; attempt++ {
		enrolled, err := enroll.Run(ctx, c.enrollOptions(serverURL, func(join tailnet.Join) {
			joinTailnetInBackground(ctx, join)
		}))
		if err == nil {
			return enrolled, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// An expired, spent, malformed, unknown or revoked code will never
		// succeed by waiting: an operator has to act. Say so once and stop
		// rather than filling the fail-closed limiter (10 per IP / 15 min).
		if enroll.Terminal(err) {
			return nil, fmt.Errorf("%s", enroll.OperatorMessage(err))
		}
		wait := backoff
		if errors.Is(err, enroll.ErrRateLimited) {
			// The limiter's window is 15 minutes; a 5-second retry is what
			// keeps it closed. Never hot-retry a 429.
			if wait < time.Minute {
				wait = time.Minute
			}
		}
		logger.Printf("enrollment attempt %d failed: %v; retrying in %s", attempt, err, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
}

const discoverUsage = `orderly-print-bridge discover — list the printers this machine can see

  Probes this box's own /24 for TCP 9100 and asks each responder for its ESC/POS
  identity, then lists the Linux USB printer nodes (/dev/usb/lp*). It prints
  what it found and exits.

  This is a LOOK, never a routing decision: the daemon prints only to the
  printer Orderly assigned it. Paste an address below into Orderly's printer
  form to make it one.
`

// runDiscover is the operator's copy of the daemon's sweep — the answer to
// "what is the printer's address?" without a subnet scanner on a venue's PC.
func runDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, discoverUsage) }
	quiet := fs.Bool("quiet", false, "print only the addresses, one per line")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := discover.Options{}
	if !*quiet {
		opts.Logf = logger.Printf
	}
	found, err := discover.Sweep(ctx, opts)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Println("No printers answered on this network, and no USB printer nodes are present.")
		fmt.Println("Check the printer is powered on, on this LAN (or plugged in), and that raw printing (port 9100) is enabled.")
		return nil
	}
	for _, c := range found {
		if *quiet {
			fmt.Println(c.Address)
			continue
		}
		label := c.Model
		if label == "" {
			label = "unidentified — answered on 9100 but did not say what it is"
		}
		fmt.Printf("%-28s  %-4s  %s\n", c.Address, c.Transport, label)
	}
	return nil
}
