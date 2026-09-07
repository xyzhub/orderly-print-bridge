// Package secret carries values that must never reach stdout, a log line or an
// error string — today just the device token (`odb_…`).
//
// The type is a string underneath so it stays cheap to pass around, but every
// path fmt or encoding/json could take to print it is overridden to redact.
// Revealing is deliberate and rare: call Reveal() at the exact moment the value
// goes on the wire or into the 0600 config file, nowhere else.
package secret

import (
	"fmt"
	"io"
)

// Redacted is what a Secret renders as everywhere except Reveal().
const Redacted = "[redacted]"

// Secret is a string that redacts itself when formatted or marshalled.
type Secret string

// Reveal returns the real value. Every call site is a deliberate disclosure.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return s == "" }

// Format implements fmt.Formatter, which fmt prefers over String/GoString/error
// for every verb — so %v, %s, %q, %+v and %#v all redact.
func (s Secret) Format(f fmt.State, _ rune) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(f, Redacted)
}

// String satisfies fmt.Stringer for the non-fmt callers (string concatenation
// through .String(), template rendering, etc).
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return Redacted
}

// MarshalJSON redacts. An accidental json.Marshal of a struct holding a token
// (a log payload, an HTTP error body) therefore cannot leak it; the config
// writer reveals explicitly via a wire struct instead.
func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return []byte(`"` + Redacted + `"`), nil
}
