package enroll

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/api/apitest"
	"github.com/xyz/orderly-print-bridge/internal/config"
	"github.com/xyz/orderly-print-bridge/internal/secret"
	"github.com/xyz/orderly-print-bridge/internal/tailnet"
)

func TestValidateCode(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"ABCDEFGHJK", "ABCDEFGHJK", false},
		{"abcdefghjk", "ABCDEFGHJK", false},
		{"ABCDE-FGHJK", "ABCDEFGHJK", false}, // operators insert separators
		{"ABCD EFGH JK", "ABCDEFGHJK", false},
		{"O0IL234567", "0011234567", false}, // Crockford transcription fixups
		{"", "", true},
		{"SHORT", "", true},
		{"ABCDEFGHJKL", "", true},
		{"ABCDEFGH!K", "", true},
	}
	for _, tc := range cases {
		got, err := ValidateCode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ValidateCode(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateCode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadSetupCodeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setup-code")
	if err := os.WriteFile(path, []byte("# written by flash.sh\nABCDEFGHJK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSetupCodeFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "ABCDEFGHJK" {
		t.Fatalf("got %q", got)
	}

	_, err = ReadSetupCodeFile(filepath.Join(dir, "absent"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing code file must report os.ErrNotExist so the caller can fall back, got %v", err)
	}
}

// Master-plan task 22 acceptance: a serial read that fails degrades to "" with
// no crash — the setup code is then the identity.
func TestSerialDegradesToEmptyWithoutCrashing(t *testing.T) {
	failRead := func(string) ([]byte, error) { return nil, os.ErrPermission }
	failRun := func(string, ...string) ([]byte, error) { return nil, errors.New("not found") }

	for _, goos := range []string{"linux", "darwin", "windows", "plan9"} {
		if got := serialFor(goos, failRead, failRun); got != "" {
			t.Errorf("serialFor(%s) with everything failing = %q, want \"\"", goos, got)
		}
	}
}

func TestSerialLinuxReadsDMIAndSkipsPlaceholders(t *testing.T) {
	read := func(p string) ([]byte, error) {
		switch p {
		case "/sys/class/dmi/id/product_serial":
			return []byte("To be filled by O.E.M.\n"), nil
		case "/sys/class/dmi/id/board_serial":
			return []byte("  CZC1234ABC \n"), nil
		}
		return nil, os.ErrNotExist
	}
	if got := serialLinux(read); got != "CZC1234ABC" {
		t.Fatalf("got %q, want the board_serial fallback CZC1234ABC", got)
	}
}

func TestSerialDarwinParsesIoreg(t *testing.T) {
	out := `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000241>
    {
      "IOPlatformSerialNumber" = "C02XY1234567"
      "IOPlatformUUID" = "AAAA"
    }`
	run := func(name string, _ ...string) ([]byte, error) {
		if name != "ioreg" {
			t.Fatalf("darwin must ask ioreg, got %q", name)
		}
		return []byte(out), nil
	}
	if got := serialDarwin(run); got != "C02XY1234567" {
		t.Fatalf("got %q", got)
	}
}

func TestSerialWindowsPrefersCimThenWmic(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "powershell" {
			return nil, errors.New("not available")
		}
		if name == "wmic" {
			return []byte("SerialNumber\r\n5CD1234ABC\r\n\r\n"), nil
		}
		t.Fatalf("unexpected command %q %v", name, args)
		return nil, nil
	}
	if got := serialWindows(run); got != "5CD1234ABC" {
		t.Fatalf("got %q", got)
	}
}

// Counsel finding 9: an expired code and an already-claimed code must NOT be
// reported the same way. Collapsing them raised false "claimed twice" alerts.
func TestExpiredAndConflictAreReportedDistinctly(t *testing.T) {
	ctx := context.Background()

	expired := apitest.New(t)
	expired.CodeExpired = true
	_, errExpired := Claim(ctx, api.New(expired.URL(), ""), api.EnrollRequest{Code: expired.Code})

	claimed := apitest.New(t)
	client := api.New(claimed.URL(), "")
	if _, err := Claim(ctx, client, api.EnrollRequest{Code: claimed.Code}); err != nil {
		t.Fatalf("the first claim must succeed: %v", err)
	}
	_, errClaimed := Claim(ctx, client, api.EnrollRequest{Code: claimed.Code})

	if !errors.Is(errExpired, ErrCodeExpired) {
		t.Fatalf("410 must map to ErrCodeExpired, got %v", errExpired)
	}
	if errors.Is(errExpired, ErrAlreadyClaimed) {
		t.Fatal("an expired code must not read as a conflict (counsel finding 9)")
	}
	if !errors.Is(errClaimed, ErrAlreadyClaimed) {
		t.Fatalf("409 must map to ErrAlreadyClaimed, got %v", errClaimed)
	}
	if errors.Is(errClaimed, ErrCodeExpired) {
		t.Fatal("a conflict must not read as expiry")
	}

	msgExpired := OperatorMessage(errExpired)
	msgClaimed := OperatorMessage(errClaimed)
	if msgExpired == msgClaimed {
		t.Fatal("the operator must not see the same sentence for expiry and for a conflict")
	}
	if !strings.Contains(msgExpired, "expired") {
		t.Errorf("the expiry message should say so: %q", msgExpired)
	}
	if !strings.Contains(strings.ToLower(msgClaimed), "already") {
		t.Errorf("the conflict message should say so: %q", msgClaimed)
	}
	// Every outcome is distinguishable.
	seen := map[string]bool{}
	for _, err := range []error{
		ErrCodeExpired, ErrAlreadyClaimed, ErrSerialConflict, ErrInvalidCode,
		ErrUnknownCode, ErrDeviceRevoked, ErrRateLimited, nil,
	} {
		m := OperatorMessage(err)
		if seen[m] {
			t.Errorf("duplicate operator message: %q", m)
		}
		seen[m] = true
	}
}

func TestRunEnrollsAndPersistsWithoutLoggingTheToken(t *testing.T) {
	srv := apitest.New(t)
	dir := t.TempDir()
	codePath := filepath.Join(dir, "setup-code")
	if err := os.WriteFile(codePath, []byte(srv.Code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.json")

	var logged strings.Builder
	cfg, err := Run(context.Background(), Options{
		ServerURL:     srv.URL(),
		ConfigPath:    cfgPath,
		SetupCodePath: codePath,
		Logf:          func(f string, a ...any) { logged.WriteString(sprintf(f, a...)) },
		Serial:        func() string { return "" }, // unreadable DMI: still enrolls
		Hostname:      func() (string, error) { return "box-01", nil },
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if cfg.DeviceID != srv.DeviceID || cfg.VenueID != srv.VenueID {
		t.Fatalf("wrong identity: %s", cfg)
	}
	if !cfg.Enrolled() {
		t.Fatal("config should carry a token")
	}
	if strings.Contains(logged.String(), srv.Token) {
		t.Fatalf("the enrollment log leaked the token: %s", logged.String())
	}

	reloaded, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Token.Reveal() != srv.Token {
		t.Fatal("the token did not persist")
	}
}

func TestRunWithoutACodeAndWithoutTheLocalPageFails(t *testing.T) {
	srv := apitest.New(t)
	dir := t.TempDir()
	_, err := Run(context.Background(), Options{
		ServerURL:      srv.URL(),
		ConfigPath:     filepath.Join(dir, "bridge.json"),
		SetupCodePath:  filepath.Join(dir, "absent"),
		AllowLocalPage: false,
	})
	if err == nil {
		t.Fatal("expected an error when no code exists and the page is disabled")
	}
	if _, enrolls, _ := srv.Counts(); enrolls != 0 {
		t.Fatalf("no code should mean no enroll attempt, got %d", enrolls)
	}
}

// The loopback page is the LAST resort — a headless box has no keyboard — but
// when it is reached it must accept a typed code.
func TestLocalCodePageAcceptsATypedCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	type res struct {
		code string
		err  error
	}
	out := make(chan res, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		c, err := ServeCodePage(ctx, ln)
		out <- res{c, err}
	}()

	// The GET renders a form.
	getResp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status %d", getResp.StatusCode)
	}

	// A bad code is rejected without ending the page.
	badResp, err := http.PostForm("http://"+addr+"/", url.Values{"code": {"NOPE"}})
	if err != nil {
		t.Fatalf("POST bad: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad code should be 400, got %d", badResp.StatusCode)
	}

	goodResp, err := http.PostForm("http://"+addr+"/", url.Values{"code": {"abcd efgh jk"}})
	if err != nil {
		t.Fatalf("POST good: %v", err)
	}
	goodResp.Body.Close()

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("ServeCodePage: %v", r.err)
		}
		if r.code != "ABCDEFGHJK" {
			t.Fatalf("got %q", r.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeCodePage never returned the submitted code")
	}
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...) + "\n"
}

// The merged handler returns TWO different 409s (enrollment.ts:161). Reading
// only the status would send an operator hunting a stolen code when the real
// fault is a duplicated box.
func TestTheTwo409sAreToldApart(t *testing.T) {
	conflict := apitest.New(t)
	conflict.SerialConflict = true
	_, err := Claim(context.Background(), api.New(conflict.URL(), ""),
		api.EnrollRequest{Code: conflict.Code, Serial: "CZC1234ABC"})

	if !errors.Is(err, ErrSerialConflict) {
		t.Fatalf("409 serial_conflict must map to ErrSerialConflict, got %v", err)
	}
	if errors.Is(err, ErrAlreadyClaimed) {
		t.Fatal("a duplicated serial is not a spent code")
	}
	if !strings.Contains(strings.ToLower(OperatorMessage(err)), "serial") {
		t.Errorf("the message should name the serial: %q", OperatorMessage(err))
	}
}

func TestRevokedAndMalformedCodesAreTheirOwnOutcomes(t *testing.T) {
	revoked := apitest.New(t)
	revoked.DeviceRevoked = true
	err := mustFailClaim(t, revoked, revoked.Code)
	if !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("403 must map to ErrDeviceRevoked, got %v", err)
	}

	unknown := apitest.New(t)
	err = mustFailClaim(t, unknown, "ZZZZZZZZZZ")
	if !errors.Is(err, ErrUnknownCode) {
		t.Fatalf("404 must map to ErrUnknownCode, got %v", err)
	}

	invalid := apitest.New(t)
	err = mustFailClaim(t, invalid, "")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("400 must map to ErrInvalidCode, got %v", err)
	}
}

// Every one of these means an operator has to act; retrying just fills the
// fail-closed limiter.
func TestTerminalOutcomesDoNotInviteARetry(t *testing.T) {
	for _, err := range []error{
		ErrCodeExpired, ErrAlreadyClaimed, ErrSerialConflict,
		ErrInvalidCode, ErrUnknownCode, ErrDeviceRevoked,
	} {
		if !Terminal(err) {
			t.Errorf("%v should be terminal", err)
		}
	}
	if Terminal(ErrRateLimited) {
		t.Error("a rate limit clears on its own — back off, do not give up")
	}
	if Terminal(errors.New("dial tcp: connection refused")) {
		t.Error("a network failure is not terminal")
	}
}

// device-token.ts mints `odb_` + 43 base64url chars. A token that cannot
// possibly authenticate must fail loudly at enrollment, not silently 401 in a
// loop forever.
func TestAMalformedTokenIsRefusedRatherThanStored(t *testing.T) {
	srv := apitest.New(t)
	srv.Token = "odb_short"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.json")
	_, err := Run(context.Background(), Options{
		ServerURL:  srv.URL(),
		ConfigPath: cfgPath,
		Code:       srv.Code,
		Serial:     func() string { return "" },
		Hostname:   func() (string, error) { return "box-01", nil },
	})
	if err == nil {
		t.Fatal("a malformed token must be refused")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("the error should name the fault: %v", err)
	}
	if strings.Contains(err.Error(), srv.Token) {
		t.Fatalf("the error leaked the token: %v", err)
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Fatal("a token that cannot authenticate must not be written to disk")
	}
}

func mustFailClaim(t *testing.T, srv *apitest.Server, code string) error {
	t.Helper()
	_, err := Claim(context.Background(), api.New(srv.URL(), ""), api.EnrollRequest{Code: code})
	if err == nil {
		t.Fatal("expected a failure")
	}
	return err
}

// Task 48, the case that is normal until S13 ships (P-17): an enrol response
// with NO tailscale block. The hook still fires — with an empty Join, so the
// "no key" line is said once — and nothing else changes.
func TestRunWithoutATailscaleBlockIsACleanNoOp(t *testing.T) {
	srv := apitest.New(t)
	dir := t.TempDir()
	codePath := filepath.Join(dir, "setup-code")
	if err := os.WriteFile(codePath, []byte(srv.Code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var joins []tailnet.Join
	cfg, err := Run(context.Background(), Options{
		ServerURL:     srv.URL(),
		ConfigPath:    filepath.Join(dir, "bridge.json"),
		SetupCodePath: codePath,
		Serial:        func() string { return "" },
		Hostname:      func() (string, error) { return "box-01", nil },
		OnTailnet:     func(j tailnet.Join) { joins = append(joins, j) },
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !cfg.Enrolled() {
		t.Fatal("a missing tailscale block must not affect enrolment")
	}
	if len(joins) != 1 {
		t.Fatalf("want the hook called once, got %d", len(joins))
	}
	if !joins[0].AuthKey.IsZero() {
		t.Fatal("there was no key on the wire, so the join must carry none")
	}
}

// And the case S13 creates: the key reaches the joiner, and reaches NOTHING
// else — not the config file, not a log line.
func TestRunPassesTheTailnetKeyToTheHookAndNeverToDisk(t *testing.T) {
	const key = "tskey-auth-NOTAREALKEY-0123456789abcdef"
	srv := apitest.New(t)
	srv.Tailscale = &api.TailscaleJoin{
		AuthKey:  secret.Secret(key),
		Hostname: "orderly-box-7",
		Tags:     []string{"tag:orderly-box"},
	}
	dir := t.TempDir()
	codePath := filepath.Join(dir, "setup-code")
	if err := os.WriteFile(codePath, []byte(srv.Code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.json")

	var logged strings.Builder
	var got tailnet.Join
	if _, err := Run(context.Background(), Options{
		ServerURL:     srv.URL(),
		ConfigPath:    cfgPath,
		SetupCodePath: codePath,
		Logf:          func(f string, a ...any) { logged.WriteString(sprintf(f, a...)) },
		Serial:        func() string { return "" },
		Hostname:      func() (string, error) { return "box-01", nil },
		OnTailnet:     func(j tailnet.Join) { got = j },
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if got.AuthKey.Reveal() != key {
		t.Fatalf("the joiner got %q, not the minted key", got.AuthKey.Reveal())
	}
	if got.Hostname != "orderly-box-7" {
		t.Fatalf("hostname = %q, want the server's choice", got.Hostname)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("THE TAILNET KEY WAS WRITTEN TO %s", cfgPath)
	}
	if strings.Contains(logged.String(), key) {
		t.Fatalf("THE TAILNET KEY REACHED A LOG LINE: %s", logged.String())
	}
}

// When the server mints a key but names no host, the box names itself after the
// serial a support engineer can read off the case.
func TestTailnetHostnameFallback(t *testing.T) {
	if got := tailnetHostname("HP  t640/ABC 123", "box", "dev_x"); got != "orderly-hp-t640-abc-123" {
		t.Fatalf("serial fallback = %q", got)
	}
	if got := tailnetHostname("", "Kitchen Box.local", "dev_x"); got != "orderly-kitchen-box-local" {
		t.Fatalf("hostname fallback = %q", got)
	}
	if got := tailnetHostname("", "", "dev_abc"); got != "orderly-dev-abc" {
		t.Fatalf("device-id fallback = %q", got)
	}
}
