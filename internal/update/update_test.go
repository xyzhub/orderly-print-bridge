package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub serves both the release API and the download URLs from one
// httptest server, which is why Updater splits APIBase from DownloadBase.
type fakeGitHub struct {
	tag        string
	binary     []byte
	sums       string // when empty, generated from binary
	apiStatus  int
	assetName  string
	hits       map[string]int
	prerelease bool
}

func (f *fakeGitHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits[r.URL.Path]++
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if f.apiStatus != 0 && f.apiStatus != http.StatusOK {
				w.WriteHeader(f.apiStatus)
				return
			}
			fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":%t,"html_url":"https://example.test"}`,
				f.tag, f.prerelease)
		case strings.HasSuffix(r.URL.Path, "/"+ChecksumFile):
			sums := f.sums
			if sums == "" {
				sum := sha256.Sum256(f.binary)
				sums = fmt.Sprintf("%s  %s\n%s  %s\n",
					hex.EncodeToString(sum[:]), f.assetName,
					strings.Repeat("0", 64), "orderly-print-bridge-linux-arm64")
			}
			fmt.Fprint(w, sums)
		case strings.HasSuffix(r.URL.Path, "/"+f.assetName):
			w.Write(f.binary)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newUpdater(t *testing.T, f *fakeGitHub, current, target string) *Updater {
	t.Helper()
	f.assetName = AssetName("linux", "amd64")
	srv := f.start(t)
	return &Updater{
		APIBase:      srv.URL,
		DownloadBase: srv.URL,
		TargetPath:   target,
		Current:      current,
		GOOS:         "linux",
		GOARCH:       "amd64",
		Logf:         t.Logf,
	}
}

func writeFakeBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orderly-print-bridge")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyInstallsAVerifiedBinary(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	u := newUpdater(t, &fakeGitHub{tag: "v1.2.0", binary: []byte("NEW BINARY")}, "1.1.0", target)

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rel.TagName != "v1.2.0" {
		t.Fatalf("tag = %q", rel.TagName)
	}
	if _, err := u.Apply(context.Background(), rel); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Fatalf("the binary was not replaced: %q", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the installed binary is not executable: %v", info.Mode())
	}
	// No staged leftovers: an unverified or half-written binary must never be
	// left lying beside the real one.
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".orderly-print-bridge-update-") {
			t.Fatalf("a staging file survived: %s", e.Name())
		}
	}
}

// THE test for this feature: a download that does not match SHA256SUMS leaves
// the running binary exactly as it was.
func TestChecksumMismatchKeepsTheOldBinary(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	f := &fakeGitHub{
		tag:    "v1.2.0",
		binary: []byte("TAMPERED BINARY"),
		// A checksum for something else entirely — a corrupted download, a
		// truncated CDN response, or a swapped asset all look like this.
		sums: strings.Repeat("a", 64) + "  " + AssetName("linux", "amd64") + "\n",
	}
	u := newUpdater(t, f, "1.1.0", target)

	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	_, err = u.Apply(context.Background(), rel)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "OLD BINARY" {
		t.Fatalf("THE UNVERIFIED BINARY WAS INSTALLED: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".orderly-print-bridge-update-") {
			t.Fatalf("the unverified download survived on disk: %s", e.Name())
		}
	}
}

// A v2 is a decision, not a download.
func TestANewMajorIsRefused(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	u := newUpdater(t, &fakeGitHub{tag: "v2.0.0", binary: []byte("NEW")}, "1.1.0", target)
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrDifferentMajor) {
		t.Fatalf("want ErrDifferentMajor, got %v", err)
	}
	if !Skippable(err) {
		t.Fatal("a new major line must be a skip, not a failure")
	}
}

func TestUpToDateIsASkip(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	u := newUpdater(t, &fakeGitHub{tag: "v1.1.0", binary: []byte("NEW")}, "1.1.0", target)
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrUpToDate) || !Skippable(err) {
		t.Fatalf("want a skippable ErrUpToDate, got %v", err)
	}
}

// GitHub's unauthenticated API is 60/hour per IP: a NATted street of venues can
// spend that, and a rate limit must never crash a nightly timer.
func TestRateLimitIsASkip(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	u := newUpdater(t, &fakeGitHub{tag: "v1.2.0", apiStatus: http.StatusForbidden}, "1.1.0", target)
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrUnavailable) || !Skippable(err) {
		t.Fatalf("want a skippable ErrUnavailable, got %v", err)
	}
}

func TestOfflineIsASkip(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	u := &Updater{
		// A port nothing listens on: the venue's uplink is down.
		APIBase:      "http://127.0.0.1:1",
		DownloadBase: "http://127.0.0.1:1",
		TargetPath:   target,
		Current:      "1.1.0",
		GOOS:         "linux", GOARCH: "amd64",
	}
	_, err := u.Check(context.Background())
	if !errors.Is(err, ErrUnavailable) || !Skippable(err) {
		t.Fatalf("want a skippable ErrUnavailable, got %v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("the binary must be untouched: %v", statErr)
	}
}

// An asset with no SHA256SUMS line is an asset nobody vouched for.
func TestAnUnchecksummedAssetIsRefused(t *testing.T) {
	target := writeFakeBinary(t, "OLD BINARY")
	f := &fakeGitHub{tag: "v1.2.0", binary: []byte("NEW"), sums: strings.Repeat("b", 64) + "  something-else\n"}
	u := newUpdater(t, f, "1.1.0", target)
	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := u.Apply(context.Background(), rel); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("want ErrNoAsset, got %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "OLD BINARY" {
		t.Fatalf("the binary changed: %q", body)
	}
}

// The asset names are a cross-repo contract (P-12) with install.sh.
func TestAssetNamesAreTheContract(t *testing.T) {
	if got := AssetName("linux", "amd64"); got != "orderly-print-bridge-linux-amd64" {
		t.Fatalf("asset name drifted to %q — install.sh 404s on that", got)
	}
	if got := AssetName("linux", "arm64"); got != "orderly-print-bridge-linux-arm64" {
		t.Fatalf("asset name drifted to %q", got)
	}
}
