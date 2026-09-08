// Package update installs a newer bridge binary from the project's public
// GitHub Releases, verified against the release's own SHA256SUMS
// (master-plan task 47).
//
// This is a remote code path onto a client's counter, so the rules are strict
// and every one of them is enforced here rather than in a runbook:
//
//   - NOTHING unverified is ever executed. The download lands in a temp file
//     beside the current binary, its SHA-256 is compared against the release's
//     SHA256SUMS entry for exactly this asset, and only then is it renamed into
//     place. A mismatch removes the temp file and leaves the running binary
//     untouched — that is a test, not a comment.
//   - It refuses anything outside the CURRENT MAJOR. A v2 is a decision an
//     operator makes after reading what changed, never something a nightly
//     timer downloads.
//   - Offline is a no-op. A venue's uplink is down, GitHub's unauthenticated
//     API rate limit is 60/hour per IP and a NATted street of restaurants can
//     hit it — both are one log line and exit 0, never a crash and never a
//     restart loop.
//   - rename(2) is the install step, so a power cut mid-update leaves either
//     the old binary or the new one, never half of either.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/version"
)

// The public repository this daemon updates from. It is public on purpose: an
// installer that needs a credential is an installer a venue's IT cannot run.
const (
	Owner = "xyzhub"
	Repo  = "orderly-print-bridge"

	// DefaultAPIBase and DefaultDownloadBase are split so a test can point both
	// at one httptest server.
	DefaultAPIBase      = "https://api.github.com"
	DefaultDownloadBase = "https://github.com"

	// ChecksumFile is the single SHA256SUMS asset covering every binary in a
	// release.
	ChecksumFile = "SHA256SUMS"

	// MaxAssetBytes bounds a download. The binary is ~8 MB; 128 MB is a
	// generous ceiling that still refuses to fill a box's disk.
	MaxAssetBytes = 128 << 20

	// HTTPTimeout bounds the whole exchange. A box on a slow venue uplink gets
	// minutes, not forever.
	HTTPTimeout = 5 * time.Minute
)

var (
	// ErrUpToDate — the latest release IS what is running.
	ErrUpToDate = errors.New("update: already running the latest release")
	// ErrDifferentMajor — the latest release is outside this binary's major
	// line. Never downloaded; an operator decides.
	ErrDifferentMajor = errors.New("update: the latest release is a new major version and will not be installed automatically")
	// ErrUnavailable — GitHub could not be reached or answered a rate limit.
	// A skip, never a failure.
	ErrUnavailable = errors.New("update: the release feed is not reachable right now")
	// ErrChecksumMismatch — the download did not match SHA256SUMS. The running
	// binary is untouched.
	ErrChecksumMismatch = errors.New("update: the downloaded binary does not match the release checksum; keeping the current binary")
	// ErrNoAsset — this release has no binary for this OS/arch.
	ErrNoAsset = errors.New("update: this release has no binary for this platform")
)

// AssetName is the release asset for one platform. It is a CROSS-REPO CONTRACT
// (P-12): `install.sh` in the Orderly repo is written against these names, so a
// rename here is a 404 there that looks like a network failure.
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("%s-%s-%s", Repo, goos, goarch)
}

// Release is the subset of GitHub's release JSON this needs.
type Release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
}

// Updater performs the check and the install. The zero value is production.
type Updater struct {
	APIBase      string // default DefaultAPIBase
	DownloadBase string // default DefaultDownloadBase
	HTTPClient   *http.Client
	// TargetPath is the binary to replace; empty means os.Executable().
	TargetPath string
	// Current is the running version; empty means version.Version.
	Current string
	// GOOS/GOARCH override the platform (tests).
	GOOS, GOARCH string
	Logf         func(format string, args ...any)
}

func (u *Updater) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

func (u *Updater) client() *http.Client {
	if u.HTTPClient != nil {
		return u.HTTPClient
	}
	return &http.Client{Timeout: HTTPTimeout}
}

func (u *Updater) apiBase() string {
	if u.APIBase != "" {
		return strings.TrimRight(u.APIBase, "/")
	}
	return DefaultAPIBase
}

func (u *Updater) downloadBase() string {
	if u.DownloadBase != "" {
		return strings.TrimRight(u.DownloadBase, "/")
	}
	return DefaultDownloadBase
}

func (u *Updater) current() string {
	if u.Current != "" {
		return u.Current
	}
	return version.Version
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

func (u *Updater) target() (string, error) {
	if u.TargetPath != "" {
		return u.TargetPath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("update: cannot find my own binary: %w", err)
	}
	// Follow the symlink so an update replaces the real file, not the link.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// Check asks for the latest release and decides whether it may be installed.
// ErrUpToDate, ErrDifferentMajor and ErrUnavailable are all NORMAL answers.
func (u *Updater) Check(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", u.apiBase(), Owner, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// The unauthenticated API allows 60 requests an hour per IP; a street
		// of venues behind one NAT can spend that. Skip, never crash.
		return nil, fmt.Errorf("%w (GitHub rate limit)", ErrUnavailable)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("%w: unreadable release feed: %v", ErrUnavailable, err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("%w: the release feed named no tag", ErrUnavailable)
	}
	if rel.Draft || rel.Prerelease {
		return nil, fmt.Errorf("%w: the latest release is a draft/prerelease", ErrUpToDate)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	if latest == strings.TrimPrefix(u.current(), "v") {
		return &rel, ErrUpToDate
	}
	if got, want := version.Major(latest), version.Major(u.current()); got != want || got == 0 {
		return &rel, fmt.Errorf("%w (running v%d, latest %s)", ErrDifferentMajor, want, rel.TagName)
	}
	return &rel, nil
}

// Apply downloads the release's asset for this platform, verifies it against
// the release's SHA256SUMS and renames it over the running binary. It returns
// the path it replaced.
func (u *Updater) Apply(ctx context.Context, rel *Release) (string, error) {
	target, err := u.target()
	if err != nil {
		return "", err
	}
	asset := AssetName(u.goos(), u.goarch())

	want, err := u.wantedDigest(ctx, rel.TagName, asset)
	if err != nil {
		return "", err
	}

	// The temp file MUST live beside the target: rename(2) does not cross
	// filesystems, and /tmp is very often a different one on a box.
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".orderly-print-bridge-update-*")
	if err != nil {
		return "", fmt.Errorf("update: cannot stage the download in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Any early return from here removes the staged file: an unverified binary
	// must never survive on disk where a future run might pick it up.
	staged := false
	defer func() {
		tmp.Close()
		if !staged {
			os.Remove(tmpPath)
		}
	}()

	sum := sha256.New()
	if err := u.download(ctx, u.assetURL(rel.TagName, asset), io.MultiWriter(tmp, sum)); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("update: cannot flush the download: %w", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("%w (expected %s, got %s)", ErrChecksumMismatch, want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return "", fmt.Errorf("update: cannot make the new binary executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("update: cannot close the download: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("update: cannot install over %s (is the bridge running as root?): %w", target, err)
	}
	staged = true
	return target, nil
}

func (u *Updater) assetURL(tag, asset string) string {
	// Pinned to the TAG the check verified, not `latest/download`: between the
	// check and the download a new release could appear, and installing a
	// version nothing verified is exactly what this package exists to prevent.
	return fmt.Sprintf("%s/%s/%s/releases/download/%s/%s", u.downloadBase(), Owner, Repo, tag, asset)
}

// wantedDigest fetches SHA256SUMS for the release and returns the line for this
// asset. No entry means no install: an asset the release did not checksum is an
// asset nobody vouched for.
func (u *Updater) wantedDigest(ctx context.Context, tag, asset string) (string, error) {
	var body strings.Builder
	if err := u.download(ctx, u.assetURL(tag, ChecksumFile), &body); err != nil {
		return "", err
	}
	for _, line := range strings.Split(body.String(), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes "<hex>  <name>" and marks binary mode with a '*'.
		if strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%w (%s is not in %s)", ErrNoAsset, asset, ChecksumFile)
}

func (u *Updater) download(ctx context.Context, url string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := u.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s is not published (HTTP 404)", ErrNoAsset, url)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered HTTP %d", ErrUnavailable, url, resp.StatusCode)
	}
	if _, err := io.Copy(dst, io.LimitReader(resp.Body, MaxAssetBytes)); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Skippable reports whether an error means "nothing to do", as opposed to
// "something went wrong". The nightly timer and the in-daemon check both exit 0
// on these: a venue's uplink being down is not an incident.
func Skippable(err error) bool {
	return errors.Is(err, ErrUpToDate) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrDifferentMajor)
}
