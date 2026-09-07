package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/secret"
)

const token = "odb_1234567890abcdef1234567890abcdef"

// Counsel finding 4, enforced in the constructor rather than trusted to every
// call site: once a byte has reached the printer, `retryable` cannot be set.
func TestFailedCannotBeRetryableAfterAByteIsWritten(t *testing.T) {
	got := Failed("prn_1", FailOffline, "connection reset mid-stream", 1, true)
	if got.Retryable {
		t.Fatal("a failure after 1 byte written must not be retryable")
	}
	if got.BytesWritten != 1 {
		t.Fatalf("bytesWritten = %d", got.BytesWritten)
	}

	// With no reason supplied, a partial write names itself.
	if r := Failed("prn_1", "", "", 99, true); r.FailureReason != FailPartial || r.Retryable {
		t.Fatalf("want a terminal failed{partial}, got %+v", r)
	}

	// A zero-byte failure keeps whatever the caller decided.
	if r := Failed("prn_1", FailOffline, "", 0, true); !r.Retryable {
		t.Fatal("a zero-byte failure may be retryable")
	}
}

// The stored column has six values (PRINT_FAILURE_REASONS); anything else is
// rewritten to `unknown` server-side, so the daemon must not invent a seventh.
func TestFailureReasonIsCoercedIntoTheServersClosedSet(t *testing.T) {
	if r := Failed("p", "unreachable", "raw", 0, true); r.FailureReason != FailUnknown {
		t.Fatalf("a word outside the six must land on unknown, got %q", r.FailureReason)
	}
	for _, known := range FailureReasons {
		if r := Failed("p", known, "raw", 0, false); r.FailureReason != known {
			t.Errorf("%q was rewritten to %q", known, r.FailureReason)
		}
	}
	// The raw driver string survives for a human.
	if r := Failed("p", FailOffline, "dial tcp 10.0.0.5:9100: i/o timeout", 0, true); r.LastError == "" {
		t.Fatal("lastError must carry the driver string")
	}
}

// The deprecated `error` alias is gone: this build is the one that lets the
// server drop it (ack.post.ts's one-release note).
func TestAckNeverSendsTheDeprecatedErrorAlias(t *testing.T) {
	for _, body := range []AckRequest{
		Printed("p", 10),
		Voided("p", "artifact_gone"),
		Failed("p", FailPartial, "reset", 12, true),
	} {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["error"]; ok {
			t.Fatalf("the ack still sends the deprecated `error` key: %s", raw)
		}
	}
}

func TestVoidedAck(t *testing.T) {
	got := Voided("prn_1", "artifact_gone: voided before it printed")
	if got.Status != StatusVoided || got.FailureReason != "" || got.Retryable {
		t.Fatalf("a voided ack carries no failure fields: %+v", got)
	}
	if got.LastError == "" {
		t.Fatal("the human-readable detail should survive")
	}
}

func TestPrintedAck(t *testing.T) {
	got := Printed("prn_1", 4096)
	if got.Status != StatusPrinted || got.BytesWritten != 4096 || got.PrinterID != "prn_1" {
		t.Fatalf("%+v", got)
	}
	if got.FailureReason != "" || got.LastError != "" || got.Retryable {
		t.Fatalf("a success ack must carry no failure fields: %+v", got)
	}
}

// The HTTP status is the authoritative discriminator, and the body's machine
// code is read tolerantly from either shape H3 might send.
func TestErrorStatusAndCodeParsing(t *testing.T) {
	cases := []struct {
		body     string
		wantCode string
	}{
		// The merged shape: the machine code is in BOTH places.
		{`{"statusCode":410,"statusMessage":"code_expired","data":{"error":"code_expired"}}`, "code_expired"},
		{`{"statusCode":409,"statusMessage":"serial_conflict","data":{"error":"serial_conflict"}}`, "serial_conflict"},
		{`{"statusCode":409,"code":"already_claimed","message":"taken"}`, "already_claimed"},
		{`plain text failure`, ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL, "").Enroll(context.Background(), EnrollRequest{Code: "ABCDEFGHJK"})
		srv.Close()
		if !IsStatus(err, http.StatusGone) {
			t.Fatalf("IsStatus missed the 410 for %q: %v", tc.body, err)
		}
		if IsStatus(err, http.StatusConflict) {
			t.Fatalf("410 must not read as 409")
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("want *Error, got %T", err)
		}
		if apiErr.Code != tc.wantCode {
			t.Errorf("code = %q, want %q", apiErr.Code, tc.wantCode)
		}
		if tc.wantCode != "" && !IsCode(err, tc.wantCode) {
			t.Errorf("IsCode missed %q", tc.wantCode)
		}
		if !strings.Contains(apiErr.Error(), PathEnroll) {
			t.Errorf("the error should name the route: %v", apiErr)
		}
	}
}

func TestAuthenticatedCallsCarryTheBearerTokenAndNothingElseDoes(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":null,"printers":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, secret.Secret(token))
	if _, err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "Bearer "+token) {
		t.Fatalf("the poll must carry the bearer token: %v", seen)
	}

	// Enrollment is unauthenticated by contract.
	seen = nil
	if _, err := New(srv.URL, "").Enroll(context.Background(), EnrollRequest{Code: "X"}); err == nil {
		// It fails on the empty token in the fake's response, which is fine —
		// what matters is that no Authorization header was sent.
		_ = err
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "auth=") {
		t.Fatalf("enroll must not send an Authorization header: %v", seen)
	}
}

func TestAuthenticatedCallWithoutATokenIsRefusedLocally(t *testing.T) {
	_, err := New("https://example.test", "").Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("want a local refusal, got %v", err)
	}
}

func TestArtifactRejectsAnEmptyOrOversizedBody(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer empty.Close()
	if _, err := New(empty.URL, secret.Secret(token)).Artifact(context.Background(), "j1"); err == nil {
		t.Fatal("an empty artifact must be an error, not a zero-byte print")
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1<<20)
		for i := 0; i < (MaxArtifactBytes/len(buf))+1; i++ {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer huge.Close()
	if _, err := New(huge.URL, secret.Secret(token)).Artifact(context.Background(), "j1"); err == nil {
		t.Fatal("an oversized artifact must be refused")
	}
}
