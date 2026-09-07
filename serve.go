package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/bridge"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/enroll"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

const serveUsage = `orderly-print-bridge serve — run the print daemon

  Enrolls if this device has no token yet (setup code + hardware serial), then
  polls Orderly every 3s for print jobs, rasterises them to ESC/POS and
  acknowledges each one.

FLAGS
  --server <url>       Orderly base URL (e.g. https://orderly-staging.fly.dev)
                       Only needed for the first enrollment; then it is stored.
  --config <path>      config file (default: the OS location, see docs)
  --setup-code <path>  per-flash setup code file (default /etc/orderly/setup-code)
  --code <code>        setup code, typed instead of read from the file
  --no-local-page      do not fall back to the setup page on 127.0.0.1:47831
  --poll <duration>    poll interval (default 3s)
  --once               run one poll cycle and exit (for smoke tests)
`

const enrollUsage = `orderly-print-bridge enroll — claim a setup code, store the token, exit

FLAGS
  --server <url>       Orderly base URL (required)
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

func (c *commonFlags) enrollOptions(serverURL string) enroll.Options {
	return enroll.Options{
		ServerURL:      serverURL,
		ConfigPath:     c.path(),
		SetupCodePath:  c.setupCode,
		Code:           c.code,
		AllowLocalPage: !c.noLocalPage,
		Logf:           logger.Printf,
	}
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
		fs.Usage()
		return errors.New("--server is required for the first enrollment")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err := enroll.Run(ctx, c.enrollOptions(serverURL))
	return err
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, serveUsage) }
	var c commonFlags
	bindCommon(fs, &c)
	poll := fs.Duration("poll", bridge.DefaultPollInterval, "poll interval")
	once := fs.Bool("once", false, "run a single poll cycle and exit")
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

	err = b.Run(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		logger.Printf("bridge %s stopped", version.Version)
		return nil
	case errors.Is(err, bridge.ErrRevoked):
		// Exit non-zero so systemd/Docker surface it, but say the operator
		// word ("revoked"), not the HTTP status.
		return err
	default:
		return err
	}
}

// loadOrEnroll returns an enrolled config, enrolling if the device has no
// token yet. A missing config on a freshly flashed box is the normal path, not
// an error.
func loadOrEnroll(ctx context.Context, c *commonFlags) (*config.Config, error) {
	cfg, err := config.LoadFrom(c.path())
	switch {
	case err == nil && cfg.Enrolled():
		if c.server != "" && c.server != cfg.ServerURL {
			logger.Printf("server URL changed to %s", c.server)
			cfg.ServerURL = c.server
			if saveErr := cfg.Save(); saveErr != nil {
				return nil, saveErr
			}
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
		return nil, errors.New("this device is not enrolled and no --server was given")
	}

	// Enrollment must not depend on Tailscale or anything else being up
	// (LD-17), but it does depend on Orderly being reachable — so retry
	// rather than exiting: a box powered on before its uplink is a normal
	// first boot, and a dead container is a box that never prints.
	backoff := 5 * time.Second
	for attempt := 1; ; attempt++ {
		enrolled, err := enroll.Run(ctx, c.enrollOptions(serverURL))
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
