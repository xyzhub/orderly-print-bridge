// Package apitest is an httptest fake of Orderly's `/api/agent/v1` surface.
//
// It exists because Phase 1 (the real handlers) was not merged when the daemon
// was built: the daemon is tested end-to-end against the WRITTEN contract
// documented at the top of internal/api/contract.go. When Phase 1 lands, this
// fake and that contract file are the two places to reconcile.
//
// The fake enforces the parts of the contract the daemon must not violate:
// enrollment is first-claim-wins (a second claim is 409, an expired code is
// 410 — never the same status), every authenticated route requires the exact
// bearer token, and the artifact is only served for a job the caller polled.
package apitest

import (
	"encoding/json"
	"fmt"
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
	Code        string // the setup code the fake accepts
	Token       string // the token it hands out
	DeviceID    string
	VenueID     string
	CodeExpired bool // 410 instead of a successful claim
	claimed     bool

	// Heartbeat state.
	printers []api.Printer
	roles    map[string]api.Role

	// Job state.
	queue     []api.Job
	artifacts map[string][]byte

	// Recorded traffic.
	heartbeats  []api.HeartbeatRequest
	acks        []Ack
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

// New starts a fake server with a default code/token pair.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		Code:           "ABCDEFGHJK",
		Token:          "odb_faketoken_0123456789",
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
		fail(w, http.StatusNotFound, "not_found", "no such route: "+r.URL.Path)
	}
}

func (s *Server) authed(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request)) {
	s.mu.Lock()
	want := "Bearer " + s.Token
	s.mu.Unlock()
	if r.Header.Get("Authorization") != want {
		fail(w, http.StatusUnauthorized, "revoked", "device token rejected")
		return
	}
	next(w, r)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "unparseable body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrollCount++
	if req.Code != s.Code {
		fail(w, http.StatusNotFound, "unknown_code", "no such setup code")
		return
	}
	// Counsel finding 9: expired must NOT look like a conflict.
	if s.CodeExpired {
		fail(w, http.StatusGone, "code_expired", "this setup code has expired")
		return
	}
	if s.claimed {
		fail(w, http.StatusConflict, "already_claimed", "this setup code was already claimed")
		return
	}
	s.claimed = true
	writeJSON(w, http.StatusOK, api.EnrollResponse{
		Token: s.Token, DeviceID: s.DeviceID, VenueID: s.VenueID, Name: "Front counter",
	})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req api.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "unparseable body")
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
		fail(w, code, "gone", "artifact is no longer available")
		return
	}
	body, ok := s.artifacts[id]
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "no artifact for "+id)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	id := jobIDFrom(r.URL.Path, "/ack")
	var req api.AckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "unparseable body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, Ack{JobID: id, Body: req})
	writeJSON(w, http.StatusOK, api.AckResponse{OK: true, Status: req.Status})
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

// fail writes an H3-shaped error body: {statusCode, statusMessage, data:{code}}.
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w,
		`{"statusCode":%d,"statusMessage":%q,"message":%q,"data":{"code":%q}}`,
		status, message, message, code)
}
