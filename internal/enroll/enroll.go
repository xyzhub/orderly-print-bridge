// Package enroll turns a per-flash setup code plus a hardware serial into a
// device token (master-plan task 22).
//
// Three properties matter more than the plumbing:
//
//   - A serial that cannot be read is "" and the setup code alone is the
//     identity. No crash, no fatal error — an unprivileged PC is a supported
//     host.
//   - `410 code_expired` and `409 already_claimed` are reported DIFFERENTLY.
//     Collapsing them is counsel finding 9: a box whose code simply aged out
//     raised a false "someone claimed this box twice" alert, and four
//     genuinely different silences all read "Never connected".
//   - The local page on 127.0.0.1:47831 is the LAST resort, never the first
//     step: a headless box has no keyboard and must enroll from the file.
package enroll

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/secret"
)

// LocalPageAddr is the loopback address of the last-resort code page.
const LocalPageAddr = "127.0.0.1:47831"

// The four outcomes a claim can have that the operator must be able to tell
// apart. Compare with errors.Is.
var (
	// ErrCodeExpired — HTTP 410. The code aged out. Mint a new one.
	ErrCodeExpired = errors.New("enroll: setup code has expired")
	// ErrAlreadyClaimed — HTTP 409. Some device already used this code.
	// THIS is the one that means "investigate"; expiry never does.
	ErrAlreadyClaimed = errors.New("enroll: setup code was already claimed")
	// ErrUnknownCode — HTTP 400/404. Mistyped or from another environment.
	ErrUnknownCode = errors.New("enroll: setup code is not recognised")
	// ErrRateLimited — HTTP 429 from the fail-closed agent-enroll limiter.
	ErrRateLimited = errors.New("enroll: too many enrollment attempts, wait and retry")
)

// Claim exchanges a setup code for a device token, mapping the contract's
// status codes onto distinguishable errors.
func Claim(ctx context.Context, client *api.Client, req api.EnrollRequest) (*api.EnrollResponse, error) {
	resp, err := client.Enroll(ctx, req)
	if err == nil {
		return resp, nil
	}
	switch {
	case api.IsStatus(err, http.StatusGone):
		return nil, fmt.Errorf("%w (%v)", ErrCodeExpired, err)
	case api.IsStatus(err, http.StatusConflict):
		return nil, fmt.Errorf("%w (%v)", ErrAlreadyClaimed, err)
	case api.IsStatus(err, http.StatusNotFound), api.IsStatus(err, http.StatusBadRequest):
		return nil, fmt.Errorf("%w (%v)", ErrUnknownCode, err)
	case api.IsStatus(err, http.StatusTooManyRequests):
		return nil, fmt.Errorf("%w (%v)", ErrRateLimited, err)
	default:
		return nil, err
	}
}

// OperatorMessage is the single line the daemon prints (and the box's console
// shows) for an enrollment failure. Every branch says something an operator
// can act on, and no two branches say the same thing.
func OperatorMessage(err error) string {
	switch {
	case err == nil:
		return "enrolled"
	case errors.Is(err, ErrCodeExpired):
		return "This box's setup code has expired. Generate a new code in Orderly (Admin › Boxes) and re-flash the code file — nothing was claimed by anyone else."
	case errors.Is(err, ErrAlreadyClaimed):
		return "This setup code was already used by another device. If that was not you, reset the device in Orderly (Admin › Boxes) to mint a fresh code."
	case errors.Is(err, ErrUnknownCode):
		return "Orderly does not recognise this setup code. Check it was generated for this environment."
	case errors.Is(err, ErrRateLimited):
		return "Orderly is rate-limiting enrollment attempts from this network. Wait 15 minutes and try again."
	default:
		return "Could not enroll with Orderly: " + err.Error()
	}
}

// Options drives Run.
type Options struct {
	// ServerURL is Orderly's base URL, e.g. https://orderly-staging.fly.dev.
	ServerURL string
	// ConfigPath is where the resulting config is written. Empty = the OS default.
	ConfigPath string
	// SetupCodePath is the per-flash code file. Empty = config.SetupCodePathLinux.
	SetupCodePath string
	// Code, when set, overrides the file (an operator running `enroll --code`).
	Code string
	// AllowLocalPage permits the last-resort loopback page when no code was
	// found. A headless box run leaves this on; a one-shot CLI enroll may not.
	AllowLocalPage bool
	// LocalPageTimeout bounds how long the page waits for a human.
	LocalPageTimeout time.Duration
	// Logf receives progress lines. Never given a token.
	Logf func(format string, args ...any)
	// Serial and Hostname are injectable for tests.
	Serial   func() string
	Hostname func() (string, error)
	// HTTPClient lets a caller supply a pre-configured api client (tests).
	Client *api.Client
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Run performs a full enrollment and returns the saved config.
func Run(ctx context.Context, opts Options) (*config.Config, error) {
	if strings.TrimSpace(opts.ServerURL) == "" && opts.Client == nil {
		return nil, errors.New("enroll: no Orderly server URL configured")
	}
	codePath := opts.SetupCodePath
	if codePath == "" {
		codePath = config.SetupCodePathLinux
	}

	code, err := resolveCode(ctx, &opts, codePath)
	if err != nil {
		return nil, err
	}

	serialFn := opts.Serial
	if serialFn == nil {
		serialFn = Serial
	}
	hostnameFn := opts.Hostname
	if hostnameFn == nil {
		hostnameFn = os.Hostname
	}
	host, err := hostnameFn()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	serial := serialFn()
	if serial == "" {
		// Not an error: an unprivileged or virtualised host has no readable
		// DMI serial and the setup code is the identity (task 22).
		opts.logf("no hardware serial readable on this host; enrolling with the setup code as the identity")
	}

	client := opts.Client
	if client == nil {
		client = api.New(opts.ServerURL, "")
	}
	resp, err := Claim(ctx, client, api.EnrollRequest{
		Code:     code,
		Hostname: host,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Serial:   serial,
	})
	if err != nil {
		opts.logf("%s", OperatorMessage(err))
		return nil, err
	}

	cfg := &config.Config{
		ServerURL: strings.TrimRight(firstNonEmpty(opts.ServerURL, client.BaseURL), "/"),
		Token:     secret.Secret(resp.Token),
		DeviceID:  resp.DeviceID,
		VenueID:   resp.VenueID,
	}
	if opts.ConfigPath != "" {
		cfg.SetPath(opts.ConfigPath)
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	// Never the token — the device id is the thing an operator quotes.
	opts.logf("enrolled as device %s (venue %s); token stored at %s", cfg.DeviceID, cfg.VenueID, cfg.Path())
	return cfg, nil
}

func resolveCode(ctx context.Context, opts *Options, codePath string) (string, error) {
	if strings.TrimSpace(opts.Code) != "" {
		return ValidateCode(opts.Code)
	}
	code, err := ReadSetupCodeFile(codePath)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		// The file exists but is unusable — say so; falling back silently
		// would hide a mis-flashed stick.
		opts.logf("setup code file unusable: %v", err)
	}
	if !opts.AllowLocalPage {
		return "", fmt.Errorf("enroll: no usable setup code at %s: %w", codePath, err)
	}
	opts.logf("no setup code on disk; opening the setup page at http://%s/ as a last resort", LocalPageAddr)
	ln, lerr := net.Listen("tcp", LocalPageAddr)
	if lerr != nil {
		return "", fmt.Errorf("enroll: no setup code at %s and the local setup page could not start: %w", codePath, lerr)
	}
	timeout := opts.LocalPageTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ServeCodePage(pctx, ln)
}

// ServeCodePage runs the last-resort code page on ln and returns the first
// valid code a human submits. It closes ln before returning.
//
// It binds loopback only and holds no secret: the code it collects is
// single-use and venue-bound, and the page never displays the device token.
func ServeCodePage(ctx context.Context, ln net.Listener) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				writePage(w, http.StatusBadRequest, "That form could not be read. Please try again.")
				return
			}
			code, err := ValidateCode(r.FormValue("code"))
			if err != nil {
				writePage(w, http.StatusBadRequest, err.Error())
				return
			}
			writeDone(w)
			select {
			case done <- result{code: code}:
			default:
			}
			return
		}
		writePage(w, http.StatusOK, "")
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case done <- result{err: err}:
			default:
			}
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("enroll: no setup code was entered on the local page: %w", ctx.Err())
	case res := <-done:
		return res.code, res.err
	}
}

const pageShell = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Orderly print bridge — setup</title></head>
<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">
<h1>Finish setting up this printer</h1>
<p>Enter the 10-character setup code from Orderly (Settings &rsaquo; Printing).</p>
%s
<form method="post">
<input name="code" autofocus autocomplete="off" spellcheck="false"
 style="font:1.25rem ui-monospace,monospace;letter-spacing:.15em;padding:.5rem;width:100%%"
 maxlength="16" dir="ltr">
<button type="submit" style="margin-top:1rem;font-size:1rem;padding:.6rem 1.2rem">Connect</button>
</form>
</body></html>`

func writePage(w http.ResponseWriter, status int, problem string) {
	banner := ""
	if problem != "" {
		banner = `<p style="color:#b91c1c"><strong>` + htmlEscape(problem) + `</strong></p>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, pageShell, banner)
}

func writeDone(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+
		`<title>Orderly print bridge — connected</title></head>`+
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">`+
		`<h1>Connecting…</h1><p>You can close this page. A welcome slip will print if a printer is set up.</p>`+
		`</body></html>`)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
