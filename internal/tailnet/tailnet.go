// Package tailnet joins this box to the venue's tailnet with the one-shot auth
// key Orderly minted during enrolment (master-plan task 48, LD-31).
//
// Three rules shape every line here:
//
//   - The key NEVER appears in argv. `/proc/<pid>/cmdline` is world-readable on
//     Linux, so `tailscale up --auth-key tskey-…` publishes the credential to
//     every process on the box for the life of the command. The key goes into a
//     0600 file that is overwritten and removed afterwards, and the command
//     line carries only `--auth-key file:/path`.
//   - The key NEVER reaches a log line, an error string or the config file. It
//     is a `secret.Secret` end to end; the one Reveal() is the write into that
//     file, and every byte of tailscale's own output is scrubbed before it is
//     wrapped in an error — `tailscale up` echoes its arguments back in some
//     failure modes.
//   - A join failure NEVER stops anything. Printing must not depend on
//     Tailscale being up (LD-17): no tailscale binary, no root, a refused key
//     or a timeout are all one warning line and a daemon that keeps printing.
package tailnet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/secret"
)

// DefaultTimeout bounds the whole join. `tailscale up` blocks until the node is
// authorized; with a pre-authorized key that is seconds, but a box whose uplink
// is down must not hold anything open for minutes.
const DefaultTimeout = 90 * time.Second

// ErrNotInstalled means there is no `tailscale` binary on PATH. On a plain
// Debian box that is simply the truth, and it is a skip, not a failure.
var ErrNotInstalled = errors.New("tailnet: no `tailscale` binary on PATH")

// ErrNoKey means the enrol response carried no Tailscale block — the normal
// case against any server older than S13 (P-17).
var ErrNoKey = errors.New("tailnet: no auth key in the enrolment response")

// Join is what the server minted, decoupled from the wire type so this package
// stays free of the api package (and of any temptation to log the struct).
type Join struct {
	AuthKey     secret.Secret
	Hostname    string
	Tags        []string
	LoginServer string
}

// Joiner runs the join. The three func fields exist so the test can drive a
// fake tailscale and assert on the exact argv, which is the only way to pin
// "the key is not on the command line" as a test rather than a comment.
type Joiner struct {
	// Binary is the command to run; empty means "tailscale".
	Binary string
	// LookPath resolves Binary; empty means exec.LookPath.
	LookPath func(string) (string, error)
	// Run executes the resolved command and returns its combined output.
	Run func(ctx context.Context, bin string, args ...string) ([]byte, error)
	// KeyDir is where the 0600 key file is written; empty means os.TempDir().
	// On a box this is tmpfs, so the key never touches the SSD.
	KeyDir string
	// Timeout bounds the join; 0 means DefaultTimeout.
	Timeout time.Duration
	// Logf receives the one line this package is allowed to say.
	Logf func(format string, args ...any)
}

func (j *Joiner) logf(format string, args ...any) {
	if j.Logf != nil {
		j.Logf(format, args...)
	}
}

func (j *Joiner) binary() string {
	if j.Binary != "" {
		return j.Binary
	}
	return "tailscale"
}

func (j *Joiner) lookPath(name string) (string, error) {
	if j.LookPath != nil {
		return j.LookPath(name)
	}
	return exec.LookPath(name)
}

func (j *Joiner) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if j.Run != nil {
		return j.Run(ctx, bin, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// A parent environment variable is not a place a key can hide either: the
	// child inherits the daemon's env, which never holds one.
	return cmd.CombinedOutput()
}

// Up joins the tailnet. It returns ErrNoKey or ErrNotInstalled for the two
// "nothing to do" cases so a caller can log them at a lower volume than a real
// failure; every return path is safe to ignore.
func (j *Joiner) Up(ctx context.Context, join Join) error {
	if join.AuthKey.IsZero() {
		j.logf("no tailnet key in the enrolment response; this box will be reachable only on its LAN")
		return ErrNoKey
	}
	bin, err := j.lookPath(j.binary())
	if err != nil {
		j.logf("tailscale is not installed on this host; skipping the tailnet join (printing is unaffected)")
		return ErrNotInstalled
	}

	keyPath, cleanup, err := j.writeKeyFile(join.AuthKey)
	if err != nil {
		return err
	}
	defer cleanup()

	timeout := j.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// WithoutCancel: enrolment's context may already be on its way out, and a
	// half-run `tailscale up` is a worse state than a completed one.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	args := Args(keyPath, join)
	out, err := j.run(runCtx, bin, args...)
	// Scrub before ANY of this is formatted: `tailscale up` echoes flags back
	// on some failures, and the file path is fine to show but its contents are
	// not — a future tailscale that reads and quotes the file must not leak it
	// through our error.
	clean := scrub(string(out), join.AuthKey)
	if err != nil {
		return fmt.Errorf("tailnet: `%s up` failed: %v: %s", j.binary(),
			scrub(err.Error(), join.AuthKey), strings.TrimSpace(clean))
	}
	name := join.Hostname
	if name == "" {
		name = "this host"
	}
	j.logf("joined the tailnet as %s", name)
	return nil
}

// Args builds the argv for `tailscale up`. Exported so the test can assert on
// the exact command line — the "no key in argv" rule is worth a test, not a
// comment.
//
// --ssh: the whole point is a support path onto a box behind a venue's NAT.
// --accept-dns=false: a box must never take DNS from the tailnet; its printer
// lives on the LAN and MagicDNS on a venue's network breaks that resolution.
func Args(keyPath string, join Join) []string {
	args := []string{
		"up",
		"--auth-key", "file:" + keyPath,
		"--ssh",
		"--accept-dns=false",
	}
	if join.Hostname != "" {
		args = append(args, "--hostname", join.Hostname)
	}
	if len(join.Tags) > 0 {
		args = append(args, "--advertise-tags="+strings.Join(join.Tags, ","))
	}
	if join.LoginServer != "" {
		args = append(args, "--login-server="+join.LoginServer)
	}
	return args
}

// writeKeyFile puts the key in a 0600 file and returns a cleanup that
// overwrites it before removing it. Overwrite-then-remove is not a claim about
// defeating a forensic read of an SSD — it is about the far likelier case: a
// tmpfs page or a backup that copies an unlinked-but-still-open file.
func (j *Joiner) writeKeyFile(key secret.Secret) (string, func(), error) {
	dir := j.KeyDir
	if dir == "" {
		dir = os.TempDir()
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", func() {}, fmt.Errorf("tailnet: could not name the key file: %w", err)
	}
	path := filepath.Join(dir, "orderly-tskey-"+hex.EncodeToString(nonce[:]))
	// O_EXCL: never follow a symlink an attacker left at a predictable name.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", func() {}, fmt.Errorf("tailnet: could not create the key file: %w", err)
	}
	body := key.Reveal() // the ONE deliberate disclosure in this package
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		os.Remove(path)
		// Never %w the write error verbatim into a message that could contain
		// the payload; os errors don't, but say the path only.
		return "", func() {}, fmt.Errorf("tailnet: could not write the key file %s", path)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", func() {}, fmt.Errorf("tailnet: could not close the key file %s", path)
	}
	cleanup := func() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0o600); err == nil {
			zeros := make([]byte, len(body))
			_, _ = f.Write(zeros)
			_ = f.Sync()
			_ = f.Close()
		}
		_ = os.Remove(path)
	}
	return path, cleanup, nil
}

// scrub removes the key from anything on its way to a log line or an error.
func scrub(s string, key secret.Secret) string {
	if key.IsZero() {
		return s
	}
	raw := key.Reveal()
	if raw == "" {
		return s
	}
	return strings.ReplaceAll(s, raw, secret.Redacted)
}
