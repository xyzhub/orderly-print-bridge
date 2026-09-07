package enroll

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// CodeLength is the setup code's length: 10 characters of Crockford base32
// (memo 4). Crockford drops I, L, O and U, so a code is filename-safe on all
// three OSes and cannot be misread off a printed slip.
const CodeLength = 10

// codePattern mirrors memo 4's regex exactly: [0-9A-HJKMNP-TV-Z]{10}.
var codePattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{10}$`)

// crockfordFixups are the transcription errors Crockford defines away. A
// human copying a code off a slip writes O for 0 and l for 1; decoding is
// specified to accept both.
var crockfordFixups = strings.NewReplacer(
	"O", "0", "o", "0",
	"I", "1", "i", "1",
	"L", "1", "l", "1",
)

// NormalizeCode uppercases, strips the separators people insert (spaces,
// dashes) and applies the Crockford transcription fixups. It does not
// validate; call ValidateCode after.
func NormalizeCode(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.NewReplacer(" ", "", "-", "", "_", "", "\t", "").Replace(s)
	s = strings.ToUpper(s)
	return crockfordFixups.Replace(s)
}

// ValidateCode returns the normalized code or an error naming what is wrong.
// The error text is safe to show an operator — it contains no secret.
func ValidateCode(raw string) (string, error) {
	code := NormalizeCode(raw)
	if code == "" {
		return "", fmt.Errorf("setup code is empty")
	}
	if len(code) != CodeLength {
		return "", fmt.Errorf("setup code must be %d characters, got %d", CodeLength, len(code))
	}
	if !codePattern.MatchString(code) {
		return "", fmt.Errorf("setup code contains a character that is not in the code alphabet")
	}
	return code, nil
}

// ReadSetupCodeFile reads and validates the per-flash setup code the flash
// script writes beside the Tailscale key (LD-10). A missing file is reported
// distinctly so the caller can fall back to the local page instead of
// treating it as a hard failure.
func ReadSetupCodeFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no setup code at %s: %w", path, os.ErrNotExist)
		}
		return "", fmt.Errorf("read setup code %s: %w", path, err)
	}
	// The file may carry a trailing newline from the flash script, and may
	// carry a comment line; take the first non-empty, non-# line.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return ValidateCode(line)
	}
	return "", fmt.Errorf("setup code file %s is empty", path)
}
