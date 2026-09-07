package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const token = "odb_9f3c1d2b4a6e8f0c1d2b3a4e5f60718293a4b5c6"

// A token that reaches stdout, a log line or an error string is a token that
// reaches a support ticket. Every formatting verb must redact.
func TestSecretRedactsThroughEveryFormattingVerb(t *testing.T) {
	s := Secret(token)
	type holder struct {
		Name  string
		Token Secret
	}
	h := holder{Name: "front counter", Token: s}

	cases := map[string]string{
		"%v direct":     fmt.Sprintf("%v", s),
		"%s direct":     fmt.Sprintf("%s", s),
		"%q direct":     fmt.Sprintf("%q", s),
		"%#v direct":    fmt.Sprintf("%#v", s),
		"%+v direct":    fmt.Sprintf("%+v", s),
		"String()":      s.String(),
		"%v struct":     fmt.Sprintf("%v", h),
		"%+v struct":    fmt.Sprintf("%+v", h),
		"%#v struct":    fmt.Sprintf("%#v", h),
		"error wrap":    fmt.Errorf("auth failed for %v", s).Error(),
		"errors.New":    errors.New(fmt.Sprint(s)).Error(),
		"concatenation": "Bearer " + s.String(),
	}
	for name, got := range cases {
		if strings.Contains(got, token) {
			t.Errorf("%s leaked the token: %q", name, got)
		}
	}
}

func TestSecretRedactsThroughJSON(t *testing.T) {
	body, err := json.Marshal(struct {
		Token Secret `json:"token"`
	}{Secret(token)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), token) {
		t.Fatalf("json.Marshal leaked the token: %s", body)
	}
	if !strings.Contains(string(body), Redacted) {
		t.Fatalf("expected the redaction marker, got %s", body)
	}
}

func TestRevealIsTheOnlyWayOut(t *testing.T) {
	s := Secret(token)
	if s.Reveal() != token {
		t.Fatalf("Reveal() must return the real value")
	}
	if !Secret("").IsZero() || Secret("x").IsZero() {
		t.Fatalf("IsZero is wrong")
	}
	if got := fmt.Sprintf("%v", Secret("")); got != "" {
		t.Fatalf("an empty secret should print as empty, got %q", got)
	}
}
