package tailnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/secret"
)

const fakeKey = "tskey-auth-kZ9NOTAREALKEY0000000-abcdefghijklmnop"

// recorder collects every byte this package could possibly emit: the log lines,
// the argv, and the error strings. The test then greps ALL of it for the key.
type recorder struct {
	lines []string
	argv  [][]string
}

func (r *recorder) logf(format string, args ...any) {
	// fmt.Sprintf, so the secret's Formatter is exercised exactly as the real
	// logger would exercise it.
	r.lines = append(r.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

// The load-bearing test (task 48): a successful join, and the key does not
// appear in argv, in a log line, or in any error.
func TestKeyNeverReachesArgvOrLogs(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	var keyFileSeen string

	j := &Joiner{
		KeyDir:   dir,
		Logf:     rec.logf,
		LookPath: func(string) (string, error) { return "/usr/bin/tailscale", nil },
		Run: func(_ context.Context, bin string, args ...string) ([]byte, error) {
			rec.argv = append(rec.argv, append([]string{bin}, args...))
			// The file must exist, be 0600 and hold the key while the command
			// runs — that is the whole substitute for putting it in argv.
			for i, a := range args {
				if a == "--auth-key" && i+1 < len(args) {
					keyFileSeen = strings.TrimPrefix(args[i+1], "file:")
				}
			}
			body, err := os.ReadFile(keyFileSeen)
			if err != nil {
				t.Fatalf("tailscale could not read the key file: %v", err)
			}
			if string(body) != fakeKey {
				t.Fatalf("the key file holds %q, not the key", string(body))
			}
			info, err := os.Stat(keyFileSeen)
			if err != nil {
				t.Fatalf("stat key file: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("key file mode is %o, want 600", perm)
			}
			// A real `tailscale up` echoes its flags back on failure; prove the
			// scrub by handing the key straight back in the output.
			return []byte("Success. auth key was " + fakeKey), nil
		},
	}

	err := j.Up(context.Background(), Join{
		AuthKey:  secret.Secret(fakeKey),
		Hostname: "orderly-box-abc123",
		Tags:     []string{"tag:orderly-box"},
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}

	// 1. argv
	if len(rec.argv) != 1 {
		t.Fatalf("want one tailscale invocation, got %d", len(rec.argv))
	}
	joined := strings.Join(rec.argv[0], " ")
	if strings.Contains(joined, fakeKey) {
		t.Fatalf("THE KEY IS IN ARGV: %s", joined)
	}
	if !strings.Contains(joined, "--auth-key file:") {
		t.Fatalf("want --auth-key file:<path>, got %s", joined)
	}
	for _, want := range []string{"up", "--ssh", "--accept-dns=false", "--hostname", "orderly-box-abc123", "--advertise-tags=tag:orderly-box"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv is missing %q: %s", want, joined)
		}
	}

	// 2. every log line
	for _, line := range rec.lines {
		if strings.Contains(line, fakeKey) {
			t.Fatalf("THE KEY IS IN A LOG LINE: %s", line)
		}
	}

	// 3. the key file is gone, and what was left behind is not the key
	if _, err := os.Stat(keyFileSeen); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the key file survived the join: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		body, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(body), fakeKey) {
			t.Fatalf("the key survived in %s", e.Name())
		}
	}
}

// A failing `tailscale up` that quotes the key back must not put it in the
// error a caller logs.
func TestFailureErrorIsScrubbed(t *testing.T) {
	j := &Joiner{
		KeyDir:   t.TempDir(),
		LookPath: func(string) (string, error) { return "/usr/bin/tailscale", nil },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("backend error: invalid key " + fakeKey), errors.New("exit status 1 (" + fakeKey + ")")
		},
	}
	err := j.Up(context.Background(), Join{AuthKey: secret.Secret(fakeKey), Hostname: "box"})
	if err == nil {
		t.Fatal("want an error from a failing join")
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("THE KEY IS IN THE ERROR: %v", err)
	}
	if !strings.Contains(err.Error(), secret.Redacted) {
		t.Fatalf("want the redaction marker in %q", err.Error())
	}
}

// The normal case until S13 ships (P-17): no Tailscale block at all.
func TestNoKeyIsACleanNoOp(t *testing.T) {
	rec := &recorder{}
	ran := false
	j := &Joiner{
		Logf:     rec.logf,
		LookPath: func(string) (string, error) { t.Fatal("must not even look for tailscale"); return "", nil },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			ran = true
			return nil, nil
		},
	}
	err := j.Up(context.Background(), Join{})
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
	if ran {
		t.Fatal("no key must never run tailscale")
	}
	if len(rec.lines) != 1 {
		t.Fatalf("want exactly one log line, got %v", rec.lines)
	}
}

// A box with no tailscale installed is a supported box.
func TestMissingBinaryIsASkip(t *testing.T) {
	rec := &recorder{}
	j := &Joiner{
		Logf:     rec.logf,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("must not run a binary that is not there")
			return nil, nil
		},
	}
	if err := j.Up(context.Background(), Join{AuthKey: secret.Secret(fakeKey)}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled, got %v", err)
	}
	if len(rec.lines) != 1 || !strings.Contains(rec.lines[0], "not installed") {
		t.Fatalf("want one 'not installed' line, got %v", rec.lines)
	}
}

func TestArgsCarryNoKey(t *testing.T) {
	args := Args("/tmp/k", Join{AuthKey: secret.Secret(fakeKey), Hostname: "h", LoginServer: "https://hs.example"})
	for _, a := range args {
		if strings.Contains(a, fakeKey) {
			t.Fatalf("argv element %q carries the key", a)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--login-server=https://hs.example") {
		t.Fatalf("a custom control server must be passed through: %v", args)
	}
}
