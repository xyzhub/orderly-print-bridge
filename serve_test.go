package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/api/apitest"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/secret"
)

// safeBuffer is a log sink a detached goroutine (the backgrounded tailnet
// join) may still be writing to while the test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureLog swaps the package logger for a buffer and restores it.
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	previous := logger
	logger = log.New(buf, "", 0)
	t.Cleanup(func() { logger = previous })
	return buf
}

// seedEnrolled writes an enrolled bridge.json in its own directory and returns
// the path.
func seedEnrolled(t *testing.T, serverURL, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.json")
	cfg := &config.Config{
		ServerURL: serverURL,
		Token:     secret.Secret(token),
		DeviceID:  "dev_seeded",
		VenueID:   "ven_seeded",
	}
	cfg.SetPath(path)
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return path
}

// Issue #10, the whole fix. A `--server` in a systemd unit outlives the
// install that wrote it; on 2026-09-21 one carrying the production URL
// re-pointed a box enrolled to STAGING on its first reboot, and the staging
// token was then presented to production 560 times.
//
// An enrolled box therefore ignores a differing --server: the stored serverUrl
// is untouched, the other host is never contacted, and one line says what to
// do instead.
func TestEnrolledBoxNeverSwitchesServerBecauseOfAFlag(t *testing.T) {
	staging := apitest.New(t)

	var otherHostCalls int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&otherHostCalls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()

	path := seedEnrolled(t, staging.URL(), staging.Token)
	buf := captureLog(t)

	c := &commonFlags{server: other.URL, configPath: path, noLocalPage: true}
	cfg, err := loadOrEnroll(context.Background(), c)
	if err != nil {
		t.Fatalf("loadOrEnroll: %v", err)
	}

	if cfg.ServerURL != staging.URL() {
		t.Fatalf("the in-memory server URL moved to %q; the enrolled one is %q", cfg.ServerURL, staging.URL())
	}
	reloaded, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ServerURL != staging.URL() {
		t.Fatalf("bridge.json was REWRITTEN to %q — that is the outage", reloaded.ServerURL)
	}
	if got := atomic.LoadInt32(&otherHostCalls); got != 0 {
		t.Fatalf("the flag's host was contacted %d time(s); an enrolled box must not present its token elsewhere", got)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", len(lines), buf.String())
	}
	for _, want := range []string{"IGNORING --server", other.URL, staging.URL(), "enroll --server"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line must name %q:\n%s", want, lines[0])
		}
	}
	// install.sh classifies the journal by substring and checks *enrolled*
	// FIRST; a line saying "enrolled" here would read as a successful install.
	if strings.Contains(lines[0], "enrolled") {
		t.Errorf("the line must not contain the installer's success substring:\n%s", lines[0])
	}
}

// A --server that merely differs by a trailing slash is the same server, and
// must not produce a scary line.
func TestEnrolledBoxIsQuietWhenTheServerMatches(t *testing.T) {
	staging := apitest.New(t)
	path := seedEnrolled(t, staging.URL(), staging.Token)
	buf := captureLog(t)

	c := &commonFlags{server: staging.URL() + "/", configPath: path, noLocalPage: true}
	if _, err := loadOrEnroll(context.Background(), c); err != nil {
		t.Fatalf("loadOrEnroll: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "" {
		t.Fatalf("want silence, got:\n%s", buf.String())
	}
}

// The pre-enrolment contract the installer relies on: with NO --server and no
// token yet, the server URL comes from `server-url` beside the config file —
// so the systemd unit can be a bare `serve` and carry no flag into the future.
func TestFirstEnrolmentReadsTheServerURLFile(t *testing.T) {
	srv := apitest.New(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.ServerURLFileName), []byte(srv.URL()+"\n"), 0o600); err != nil {
		t.Fatalf("write server-url: %v", err)
	}
	path := filepath.Join(dir, "bridge.json")
	buf := captureLog(t)

	c := &commonFlags{configPath: path, code: srv.Code, noLocalPage: true}
	cfg, err := loadOrEnroll(context.Background(), c)
	if err != nil {
		t.Fatalf("loadOrEnroll: %v", err)
	}
	if cfg.ServerURL != srv.URL() {
		t.Fatalf("enrolled against %q, want the URL from the file %q", cfg.ServerURL, srv.URL())
	}
	if !cfg.Enrolled() {
		t.Fatal("no token was stored")
	}
	if _, enrolls, _ := srv.Counts(); enrolls != 1 {
		t.Fatalf("want exactly one enrollment call, got %d", enrolls)
	}
	if !strings.Contains(buf.String(), config.ServerURLFileName) {
		t.Errorf("the daemon must say where the URL came from:\n%s", buf.String())
	}
}

// No flag, no file: the existing error, unchanged in the part an operator (and
// the docs) recognise.
func TestFirstEnrolmentWithoutAServerURLStillFails(t *testing.T) {
	captureLog(t)
	c := &commonFlags{configPath: filepath.Join(t.TempDir(), "bridge.json"), noLocalPage: true}
	_, err := loadOrEnroll(context.Background(), c)
	if err == nil {
		t.Fatal("want an error when there is no server URL anywhere")
	}
	if !strings.Contains(err.Error(), "not enrolled and no --server was given") {
		t.Fatalf("error changed shape: %v", err)
	}
}

// A server-url file that is not a single http(s) URL is IGNORED, not
// half-trusted: it decides which Orderly a setup code is presented to.
func TestUnusableServerURLFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.ServerURLFileName), []byte("orderly.example\n"), 0o600); err != nil {
		t.Fatalf("write server-url: %v", err)
	}
	buf := captureLog(t)
	c := &commonFlags{configPath: filepath.Join(dir, "bridge.json"), noLocalPage: true}
	_, err := loadOrEnroll(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "no --server was given") {
		t.Fatalf("want the not-enrolled error, got %v", err)
	}
	if !strings.Contains(buf.String(), "http(s) URL") {
		t.Errorf("the daemon must say why the file was ignored:\n%s", buf.String())
	}
}
