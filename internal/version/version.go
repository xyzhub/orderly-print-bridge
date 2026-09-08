// Package version holds the single agent version. It is reported on every
// heartbeat (master-plan task 21) so the manager page can show which build a
// venue is running, and it is what the self-updater compares a release tag
// against (master-plan task 47).
package version

import "strings"

// Version is the bridge build identity sent to Orderly on every heartbeat.
//
// It is a VAR, not a const, because release.yml stamps the tag into it:
//
//	-ldflags "-X github.com/xyz/orderly-print-bridge/internal/version.Version=1.1.0"
//
// A `-X` on a const is silently ignored by the linker, which is how a release
// binary ends up reporting the developer's placeholder for a year. The default
// below is what an unstamped `go build` produces; bump it in the same commit as
// any behaviour change the server may need to reason about.
var Version = "1.1.0"

// UserAgent is the HTTP User-Agent every agent call carries. A func, not a
// const, so it follows a stamped Version instead of freezing the default at
// compile time.
func UserAgent() string { return "orderly-print-bridge/" + Version }

// Major returns the leading major number of Version (1 for "1.1.0", "v1.1.0"
// and "1.1.0-dev"), or 0 when it cannot be read.
//
// The self-updater refuses anything outside this major line: a v2 is a decision
// an operator makes, never something a nightly timer downloads onto a counter.
func Major(v string) int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, ".-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return 0
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
