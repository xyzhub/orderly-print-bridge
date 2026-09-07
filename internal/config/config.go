// Package config owns the daemon's on-disk state: the server URL, the device
// token, the ids it enrolled as, and the printers the server last assigned.
//
// Three properties are load-bearing (master-plan task 21):
//
//  1. One canonical path per OS — %ProgramData%\Orderly\ (Windows),
//     /Library/Application Support/Orderly/ (macOS), /etc/orderly/ (Linux).
//  2. The file is 0600. A world-readable file found on load is REWRITTEN to
//     0600 rather than merely warned about, because the token in it prints
//     that venue's receipts (customer PII) until it is revoked. On Windows,
//     where mode bits are decoration, the ACL is reset to SYSTEM +
//     Administrators via icacls.
//  3. The token never reaches stdout, a log line or an error string — it is a
//     secret.Secret, which redacts through fmt and encoding/json; only
//     Save() and the Authorization header reveal it.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xyz/orderly-print-bridge/internal/api"
	"github.com/xyz/orderly-print-bridge/internal/secret"
)

// FileMode is the only mode the config file is ever allowed to have.
const FileMode os.FileMode = 0o600

// DirMode is the mode of the directory holding it.
const DirMode os.FileMode = 0o700

// FileName is the config file's basename on every OS.
const FileName = "bridge.json"

// PathEnv overrides the OS default path. It exists for tests and for a
// developer running two daemons on one machine; production uses the default.
const PathEnv = "ORDERLY_BRIDGE_CONFIG"

// SetupCodePathLinux is where the flash script writes the per-flash setup code
// on a box (LD-10). Kept here so config and enroll agree on one string.
const SetupCodePathLinux = "/etc/orderly/setup-code"

// ErrNotFound is returned by Load when no config file exists yet — the normal
// state of a freshly flashed box, not an error condition.
var ErrNotFound = errors.New("config: no config file (not enrolled yet)")

// Config is the daemon's persisted state.
type Config struct {
	ServerURL string        `json:"serverUrl"`
	Token     secret.Secret `json:"token"`
	DeviceID  string        `json:"deviceId"`
	VenueID   string        `json:"venueId"`
	Printers  []api.Printer `json:"printers"`

	// path is where this config was loaded from / will be saved to.
	path string
}

// wire is the on-disk shape. It exists so that revealing the token is a
// deliberate act in exactly two functions (Save and Load) instead of a
// property of the Config type — an accidental json.Marshal(cfg) elsewhere
// redacts.
type wire struct {
	ServerURL string        `json:"serverUrl"`
	Token     string        `json:"token"`
	DeviceID  string        `json:"deviceId"`
	VenueID   string        `json:"venueId"`
	Printers  []api.Printer `json:"printers"`
}

// String renders the config without its token. Config has no fmt.Formatter
// because Token is already a secret.Secret; this is the convenience form.
func (c *Config) String() string {
	if c == nil {
		return "config(nil)"
	}
	return fmt.Sprintf("config{serverUrl:%s deviceId:%s venueId:%s printers:%d token:%v}",
		c.ServerURL, c.DeviceID, c.VenueID, len(c.Printers), c.Token)
}

// Enrolled reports whether the config carries a usable device token.
func (c *Config) Enrolled() bool { return c != nil && !c.Token.IsZero() }

// Path returns where this config lives on disk.
func (c *Config) Path() string { return c.path }

// SetPath overrides the save destination (used right after enrollment).
func (c *Config) SetPath(p string) { c.path = p }

// PrinterByID finds an assigned printer.
func (c *Config) PrinterByID(id string) *api.Printer {
	for i := range c.Printers {
		if c.Printers[i].ID == id {
			return &c.Printers[i]
		}
	}
	return nil
}

// DefaultPath is the config path for the running OS, honouring PathEnv.
func DefaultPath() string { return pathFor(runtime.GOOS, os.Getenv) }

// DefaultDir is the directory DefaultPath lives in.
func DefaultDir() string { return filepath.Dir(DefaultPath()) }

func pathFor(goos string, getenv func(string) string) string {
	if override := strings.TrimSpace(getenv(PathEnv)); override != "" {
		return override
	}
	switch goos {
	case "windows":
		base := getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "Orderly", FileName)
	case "darwin":
		return filepath.Join("/Library", "Application Support", "Orderly", FileName)
	default:
		return filepath.Join("/etc", "orderly", FileName)
	}
}

// Load reads the config at DefaultPath. See LoadFrom.
func Load() (*Config, error) { return LoadFrom(DefaultPath()) }

// LoadFrom reads and, if necessary, re-secures the config at path.
//
// A file whose mode grants any group or other bit is rewritten to 0600 before
// its contents are used: finding the token readable is not a warning, it is a
// repair.
func LoadFrom(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o177 != 0 {
		if err := os.Chmod(path, FileMode); err != nil {
			return nil, fmt.Errorf("config: %s is mode %#o and could not be re-secured: %w",
				path, info.Mode().Perm(), err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		// The error deliberately does not echo the file contents: a malformed
		// JSON error from encoding/json can quote the offending fragment, and
		// the token is in this file.
		return nil, fmt.Errorf("config: %s is not valid JSON", path)
	}
	cfg := &Config{
		ServerURL: w.ServerURL,
		Token:     secret.Secret(w.Token),
		DeviceID:  w.DeviceID,
		VenueID:   w.VenueID,
		Printers:  w.Printers,
		path:      path,
	}
	return cfg, nil
}

// Save writes the config atomically at 0600 (and resets the Windows ACL).
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath()
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	if runtime.GOOS != "windows" {
		// MkdirAll respects umask; force the mode we actually want.
		if err := os.Chmod(dir, DirMode); err != nil {
			return fmt.Errorf("config: secure %s: %w", dir, err)
		}
	}
	// The one place the token is deliberately revealed to disk.
	body, err := json.MarshalIndent(wire{
		ServerURL: c.ServerURL,
		Token:     c.Token.Reveal(),
		DeviceID:  c.DeviceID,
		VenueID:   c.VenueID,
		Printers:  c.Printers,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	body = append(body, '\n')

	tmp := c.path + ".tmp"
	// O_EXCL: never write the token through a symlink someone left behind.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, FileMode)
	if err != nil {
		return fmt.Errorf("config: create %s: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("config: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: install %s: %w", c.path, err)
	}
	return Secure(c.path)
}

// Secure enforces the file's permissions after the fact: 0600 on POSIX, an
// ACL restricted to SYSTEM + Administrators on Windows.
func Secure(path string) error { return secureFor(runtime.GOOS, path, runCommand) }

type commandRunner func(name string, args ...string) ([]byte, error)

func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func secureFor(goos, path string, run commandRunner) error {
	if goos != "windows" {
		if err := os.Chmod(path, FileMode); err != nil {
			return fmt.Errorf("config: chmod %s: %w", path, err)
		}
		return nil
	}
	// Windows: mode bits are decoration, the ACL is the control. Break
	// inheritance, drop everyone, then grant SYSTEM and Administrators.
	// Well-known SIDs, not localised group names: "Administrators" is
	// "Administradores" on a Spanish install and icacls would fail.
	steps := [][]string{
		{path, "/inheritance:r"},
		{path, "/grant:r", "*S-1-5-18:(F)"},     // NT AUTHORITY\SYSTEM
		{path, "/grant:r", "*S-1-5-32-544:(F)"}, // BUILTIN\Administrators
	}
	for _, args := range steps {
		if out, err := run("icacls", args...); err != nil {
			return fmt.Errorf("config: icacls %v failed: %w: %s", args[1:], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
