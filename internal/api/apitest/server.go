// Package apitest is an httptest fake of Orderly's `/api/agent/v1` surface,
// reconciled 2026-09-07 against the MERGED handlers (Orderly branch
// `mission/print-bridge-p1` at 9b6d6de3) — see internal/api/contract.go for
// the file-by-file source list.
//
// It reproduces the shapes the daemon must survive, not just the happy path:
// h3 error bodies `{statusCode, statusMessage, data:{error}}` with the machine
// code in BOTH places, the six enrollment status/code pairs (including the two
// distinct 409s), a device token that matches `odb_` + 43 base64url chars, and
// the artifact's 403 / 409 lease_lost / 410 / 501 / 502 ladder.
package apitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xyz/orderly-print-bridge/internal/api"
)

// Server is a running fake. Close it with t.Cleanup (New does that for you).
type Server struct {
	HTTP *httptest.Server

	mu sync.Mutex

	// Enrollment state.
	Code     string // the setup code the fake accepts
	Token    string // the token it hands out
	DeviceID string
	VenueID  string
	// Each of these forces one of enrollment.ts's failure branches.
	CodeExpired    bool // 410 code_expired
	SerialConflict bool // 409 serial_conflict (NOT already_claimed)
	DeviceRevoked  bool // 403 device_revoked
	claimed        bool
	// Tailscale, when set, is returned in the enrol response — the S13 server
	// half (master-plan task 48). nil is what every server before it answers,
	// and the daemon must treat that as normal.
	Tailscale *api.TailscaleJoin

	// Heartbeat state.
	printers []api.Printer
	roles    map[string]api.Role

	// Job state.
	queue     []api.Job
	artifacts map[string][]byte

	// Recorded traffic.
	heartbeats  []api.HeartbeatRequest
	acks        []Ack
	ackBodies   []map[string]any
	pollCount   int
	artifactHit map[string]int
	enrollCount int
	// ArtifactStatus, when set for a job id, is returned instead of the bytes.
	ArtifactStatus map[string]int
}

// Ack is one recorded acknowledgement.
type Ack struct {
	JobID string
	Body  api.AckRequest
}

// RawAcks returns the acknowledgements as raw JSON maps, so a test can assert
// on the KEYS the daemon sent — specifically that the deprecated `error` alias
// is gone.
func (s *Server) RawAcks() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.ackBodies...)
}

// New starts a fake server with a default code/token pair.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		Code: "ABCDEFGHJK",
		// Exactly the shape device-token.ts mints: odb_ + 43 base64url chars.
		Token:          "odb_" + strings.Repeat("A", 42) + "Z",
		DeviceID:       "dev_fake",
		VenueID:        "ven_fake",
		artifacts:      map[string][]byte{},
		artifactHit:    map[string]int{},
		ArtifactStatus: map[string]int{},
		roles:          map[string]api.Role{},
	}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.route))
	t.Cleanup(s.HTTP.Close)
	return s
}

// URL is the base URL to hand the api client.
func (s *Server) URL() string { return s.HTTP.URL }

// SetPrinters replaces what the heartbeat reports as assigned.
func (s *Server) SetPrinters(p ...api.Printer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.printers = p
}

// Enqueue adds a job the next poll will hand out, with its artifact bytes.
func (s *Server) Enqueue(job api.Job, artifact []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, job)
	if artifact != nil {
		s.artifacts[job.ID] = artifact
	}
}

// Acks returns a copy of every acknowledgement received, in order.
func (s *Server) Acks() []Ack {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ack(nil), s.acks...)
}

// Heartbeats returns a copy of every heartbeat received, in order.
func (s *Server) Heartbeats() []api.HeartbeatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.HeartbeatRequest(nil), s.heartbeats...)
}

// Counts reports poll / enroll counts and per-job artifact fetches.
func (s *Server) Counts() (polls, enrolls int, artifacts map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.artifactHit {
		out[k] = v
	}
	return s.pollCount, s.enrollCount, out
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == api.PathEnroll:
		s.handleEnroll(w, r)
	case r.URL.Path == api.PathHeartbeat:
		s.authed(w, r, s.handleHeartbeat)
	case r.URL.Path == api.PathPoll:
		s.authed(w, r, s.handlePoll)
	case strings.HasSuffix(r.URL.Path, "/artifact"):
		s.authed(w, r, s.handleArtifact)
	case strings.HasSuffix(r.URL.Path, "/ack"):
		s.authed(w, r, s.handleAck)
	default:
		fail(w, http.StatusNotFound, "not_found")
	}
}

func (s *Server) authed(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	want := "Bearer " + s.Token
	s.mu.Unlock()
	if r.Header.Get("Authorization") != want {
		fail(w, http.StatusUnauthorized, api.CodeDeviceRevoked)
		return
	}
	next(w, r)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, api.CodeInvalidCode)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrollCount++
	if strings.TrimSpace(req.Code) == "" {
		fail(w, http.StatusBadRequest, api.CodeInvalidCode)
		return
	}
	if req.Code != s.Code {
		fail(w, http.StatusNotFound, api.CodeUnknownCode)
		return
	}
	if s.DeviceRevoked {
		fail(w, http.StatusForbidden, api.CodeDeviceRevoked)
		return
	}
	// Counsel finding 9: expired must NOT look like a conflict.
	if s.CodeExpired {
		fail(w, http.StatusGone, api.CodeCodeExpired)
		return
	}
	// Two DIFFERENT 409s — the code was spent, or the hardware is duplicated.
	if s.SerialConflict {
		fail(w, http.StatusConflict, api.CodeSerialConflict)
		return
	}
	if s.claimed {
		fail(w, http.StatusConflict, api.CodeAlreadyClaimed)
		return
	}
	s.claimed = true
	body := map[string]any{
		"token": s.Token, "deviceId": s.DeviceID, "venueId": s.VenueID, "name": "Front counter",
	}
	if s.Tailscale != nil {
		// Hand-built, because api.TailscaleJoin.AuthKey is a secret.Secret and
		// MARSHALS as "[redacted]" by design — the real server is TypeScript, so
		// only this fake ever needs to put a raw key on the wire.
		body["tailscale"] = map[string]any{
			"authKey":     s.Tailscale.AuthKey.Reveal(),
			"hostname":    s.Tailscale.Hostname,
			"tags":        s.Tailscale.Tags,
			"loginServer": s.Tailscale.LoginServer,
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req api.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats = append(s.heartbeats, req)
	writeJSON(w, http.StatusOK, api.HeartbeatResponse{
		Printers: append([]api.Printer(nil), s.printers...),
		Roles:    s.roles,
	})
}

func (s *Server) handlePoll(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCount++
	if len(s.queue) == 0 {
		writeJSON(w, http.StatusOK, api.PollResponse{})
		return
	}
	job := s.queue[0]
	s.queue = s.queue[1:]
	writeJSON(w, http.StatusOK, api.PollResponse{Job: &job})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	id := jobIDFrom(r.URL.Path, "/artifact")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifactHit[id]++
	if code, ok := s.ArtifactStatus[id]; ok {
		fail(w, code, artifactCode(code))
		return
	}
	body, ok := s.artifacts[id]
	if !ok {
		fail(w, http.StatusForbidden, api.CodeJobNotOwned)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	id := jobIDFrom(r.URL.Path, "/ack")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req api.AckRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, Ack{JobID: id, Body: req})
	s.ackBodies = append(s.ackBodies, asMap)
	writeJSON(w, http.StatusOK, api.AckResponse{OK: true, Status: req.Status})
}

// artifactCode mirrors artifact.get.ts's status → machine code ladder.
func artifactCode(status int) string {
	switch status {
	case http.StatusForbidden:
		return api.CodeJobNotOwned
	case http.StatusConflict:
		return api.CodeLeaseLost
	case http.StatusGone:
		return api.CodeArtifactGone
	case http.StatusNotImplemented:
		return api.CodeNotRenderable
	case http.StatusBadGateway:
		return api.CodeRenderFailed
	default:
		return "unknown"
	}
}

func jobIDFrom(path, suffix string) string {
	trimmed := strings.TrimSuffix(path, suffix)
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return trimmed
	}
	return trimmed[i+1:]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fail writes exactly what the merged handlers throw: an h3 error whose
// MACHINE CODE appears in both `statusMessage` and `data.error`.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w,
		`{"statusCode":%d,"statusMessage":%q,"data":{"error":%q}}`,
		status, code, code)
}
