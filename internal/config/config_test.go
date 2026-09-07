package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/secret"
)

const testToken = "odb_3b1f8c2d4e5a6b7c8d9e0f1a2b3c4d5e"

func TestDefaultPathPerOS(t *testing.T) {
	empty := func(string) string { return "" }
	cases := []struct {
		goos   string
		getenv func(string) string
		want   string
	}{
		{"linux", empty, "/etc/orderly/bridge.json"},
		{"darwin", empty, "/Library/Application Support/Orderly/bridge.json"},
		{"windows", func(k string) string {
			if k == "ProgramData" {
				return `C:\ProgramData`
			}
			return ""
		}, filepath.Join(`C:\ProgramData`, "Orderly", "bridge.json")},
		{"windows", empty, filepath.Join(`C:\ProgramData`, "Orderly", "bridge.json")},
	}
	for _, tc := range cases {
		if got := pathFor(tc.goos, tc.getenv); got != tc.want {
			t.Errorf("pathFor(%s) = %q, want %q", tc.goos, got, tc.want)
		}
	}

	override := func(k string) string {
		if k == PathEnv {
			return "/tmp/custom/bridge.json"
		}
		return ""
	}
	if got := pathFor("linux", override); got != "/tmp/custom/bridge.json" {
		t.Errorf("%s override ignored: %q", PathEnv, got)
	}
}

func TestSaveWritesZeroSixHundred(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	path := filepath.Join(t.TempDir(), "orderly", "bridge.json")
	cfg := &Config{ServerURL: "https://example.test", Token: secret.Secret(testToken), DeviceID: "dev_1", VenueID: "ven_1"}
	cfg.SetPath(path)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != FileMode {
		t.Fatalf("config file is %#o, want %#o", got, FileMode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != DirMode {
		t.Fatalf("config dir is %#o, want %#o", got, DirMode)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the temp file survived the atomic install")
	}
}

// Acceptance for master-plan task 21: "a world-readable file is rewritten to
// 0600 on load".
func TestLoadRepairsAWorldReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	path := filepath.Join(t.TempDir(), "bridge.json")
	body := fmt.Sprintf(`{"serverUrl":"https://example.test","token":%q,"deviceId":"dev_1","venueId":"ven_1"}`, testToken)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != FileMode {
		t.Fatalf("load left the file at %#o, want %#o", got, FileMode)
	}
	if cfg.Token.Reveal() != testToken {
		t.Fatalf("token did not survive the repair")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	in := &Config{
		ServerURL: "https://orderly-staging.fly.dev",
		Token:     secret.Secret(testToken),
		DeviceID:  "dev_abc",
		VenueID:   "ven_xyz",
		Printers: []api.Printer{{
			ID: "prn_1", Name: "Front counter", Transport: api.TransportTCP,
			Address: "192.168.1.50:9100", WidthDots: 512, DPI: 180, BandHeight: 128, Threshold: 128,
		}},
	}
	in.SetPath(path)
	if err := in.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.ServerURL != in.ServerURL || out.DeviceID != in.DeviceID || out.VenueID != in.VenueID {
		t.Fatalf("round trip lost fields: %s", out)
	}
	if out.Token.Reveal() != testToken {
		t.Fatalf("round trip lost the token")
	}
	if len(out.Printers) != 1 || out.Printers[0] != in.Printers[0] {
		t.Fatalf("round trip lost the printers: %+v", out.Printers)
	}
	if !out.Enrolled() {
		t.Fatalf("a config with a token must read as enrolled")
	}
	if got := out.PrinterByID("prn_1"); got == nil || got.WidthDots != 512 {
		t.Fatalf("PrinterByID missed the printer")
	}
	if out.PrinterByID("nope") != nil {
		t.Fatalf("PrinterByID invented a printer")
	}
}

// The token is allowed on disk and in the Authorization header. Nowhere else.
func TestTokenNeverReachesAStringOrALogLine(t *testing.T) {
	cfg := &Config{ServerURL: "https://example.test", Token: secret.Secret(testToken), DeviceID: "dev_1"}
	renders := []string{
		cfg.String(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%s", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", *cfg),
		fmt.Errorf("could not poll with %v", cfg).Error(),
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	renders = append(renders, string(body))
	for i, got := range renders {
		if strings.Contains(got, testToken) {
			t.Errorf("render %d leaked the token: %s", i, got)
		}
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	_, err := LoadFrom(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// A malformed config must not echo its own contents: encoding/json's syntax
// errors can quote the offending fragment, and the token lives in this file.
func TestLoadOfGarbageDoesNotEchoTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(path, []byte(`{"token":"`+testToken+`" NOT JSON`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("the parse error leaked the token: %v", err)
	}
}

// On Windows the mode bits are decoration; the ACL is the control. Verify the
// icacls invocation without needing a Windows host.
func TestSecureOnWindowsResetsTheACLByWellKnownSID(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	if err := secureFor("windows", `C:\ProgramData\Orderly\bridge.json`, run); err != nil {
		t.Fatalf("secureFor: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("want 3 icacls calls, got %d: %v", len(calls), calls)
	}
	joined := fmt.Sprint(calls)
	for _, want := range []string{"icacls", "/inheritance:r", "*S-1-5-18:(F)", "*S-1-5-32-544:(F)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("icacls invocation missing %q: %v", want, calls)
		}
	}
	// Localised group names would fail on a non-English install.
	if strings.Contains(joined, "Administrators:") {
		t.Errorf("use the well-known SID, not the localised group name: %v", calls)
	}
}

func TestSecureOnWindowsSurfacesAFailure(t *testing.T) {
	run := func(string, ...string) ([]byte, error) { return []byte("Access is denied."), errors.New("exit 5") }
	err := secureFor("windows", `C:\x\bridge.json`, run)
	if err == nil {
		t.Fatal("an ACL that could not be applied must be an error, not a shrug")
	}
}
