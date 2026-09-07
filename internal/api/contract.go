// Package api holds EVERY request and response type the bridge exchanges with
// Orderly's `/api/agent/v1` surface, deliberately in this one file, so that
// reconciling the daemon against the merged Phase-1 handlers is a one-file
// diff rather than an archaeology exercise.
//
// # Contract source
//
// Phase 1 (the Orderly server side) was NOT merged when this package was
// written, so these types are built to the WRITTEN CONTRACT, not to shipped
// handlers. When Phase 1 lands, diff this file against the handlers under
// `server/api/agent/v1/` in the Orderly repo and fix the drift here.
//
// The contract is, in the Orderly repo:
//
//   - docs/product/decisions/2026-09-07-print-bridge-memos.md — the
//     "Endpoints (auth per route)" table and the schema sketch above it
//     (PrintDevice / DevicePrinter / PrinterRole / PrintJob field names).
//   - .plans/print-bridge.sessions.md — brief S3 (enrollment by serial +
//     setup code; `410 code_expired` distinct from `409 already_claimed`;
//     token `odb_…` bearer) and brief S4 (poll / artifact / ack shapes and
//     the `retryable` semantics).
//   - docs/product/decisions/2026-09-07-print-bridge-counsel.md — finding 4
//     (any byte written ⇒ terminal `failed{partial}`, never a re-queue),
//     finding 6 (never blind-print to "first printer found"), finding 9
//     (`expired` must not be reported as `conflict`).
//
// # Ambiguities resolved here (name them when reconciling)
//
//  1. The ack body is `{status, failureReason?, retryable?, bytesWritten?}`.
//     Memo 4's table writes the failure field as `error`; the S4 brief calls
//     it a "coarse failureReason list". The bridge sends BOTH keys with the
//     same coarse code so either server field name binds; drop the loser once
//     the merged handler is readable.
//  2. `job.dots` (memo) is the width the ARTIFACT is rendered at;
//     `printer.widthDots` is the physical head. The render_failed check
//     compares the decoded PNG against `printer.widthDots`, because that is
//     the number that decides whether paper is wasted.
//  3. Error bodies are read tolerantly (H3 emits
//     `{statusCode, statusMessage, data}`), but the HTTP STATUS is the
//     authoritative discriminator: 410 = code_expired, 409 = already_claimed.
package api

import "time"

// Paths on the Orderly server. Kept together so a route rename is one edit.
const (
	PathEnroll    = "/api/agent/v1/enroll"
	PathHeartbeat = "/api/agent/v1/heartbeat"
	PathPoll      = "/api/agent/v1/jobs/poll"
	// PathArtifact and PathAck take the job id: fmt.Sprintf(PathArtifact, id).
	PathArtifact = "/api/agent/v1/jobs/%s/artifact"
	PathAck      = "/api/agent/v1/jobs/%s/ack"
)

// Job kinds. `welcome` and `test` are the two slips (planner Q3).
const (
	KindReceipt = "receipt"
	KindKitchen = "kitchen"
	KindWelcome = "welcome"
	KindTest    = "test"
)

// Ack statuses (PrintJob.status terminal values the device may write).
const (
	StatusPrinted = "printed"
	StatusFailed  = "failed"
	StatusVoided  = "voided"
)

// Coarse failure reasons. The server stores these verbatim in
// `PrintJob.lastError`; the manager surface maps them to human words (S5's
// vocabulary constant), so this list stays short and stable.
const (
	// FailPartial — bytes reached the printer before the write failed.
	// TERMINAL by contract (counsel finding 4): never retryable, because a
	// retry after a partial write is how a venue gets two legal invoices.
	FailPartial = "partial"
	// FailRenderFailed — the artifact was unusable at this printer's width.
	// Never resample: a silently rescaled receipt is a quality regression the
	// venue only discovers on paper.
	FailRenderFailed = "render_failed"
	// FailUnreachable — zero bytes written; the printer never answered.
	// The only class the bridge marks retryable.
	FailUnreachable = "unreachable"
	// FailTimeout — the job did not finish inside the 60 s ack window. Bytes
	// written is unknown, so this is terminal, not retryable.
	FailTimeout = "timeout"
	// FailNoPrinter — no printer is assigned, or (for a welcome slip) no
	// candidate could be identified safely. Terminal; the manager re-enqueues.
	FailNoPrinter = "no_printer"
	// FailArtifactGone — the artifact returned 410 (voided or expired).
	FailArtifactGone = "artifact_gone"
)

// Transport values on DevicePrinter.transport.
const (
	TransportTCP = "tcp"
	TransportUSB = "usb"
)

// ---------------------------------------------------------------------------
// Enrollment — POST /api/agent/v1/enroll (unauthenticated, IP rate-limited)
// ---------------------------------------------------------------------------

// EnrollRequest is the box/PC claim. `Serial` is optional: a Linux box without
// root, or a host whose DMI is unreadable, sends "" and the setup code alone is
// the identity (master-plan task 22).
type EnrollRequest struct {
	Code     string `json:"code"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Serial   string `json:"serial,omitempty"`
	Version  string `json:"version,omitempty"`
}

// EnrollResponse is returned exactly once per code (first-claim-wins CAS).
type EnrollResponse struct {
	Token    string `json:"token"` // `odb_…`; the caller wraps it in secret.Secret immediately
	DeviceID string `json:"deviceId"`
	VenueID  string `json:"venueId"`
	Name     string `json:"name,omitempty"`
}

// ---------------------------------------------------------------------------
// Heartbeat — POST /api/agent/v1/heartbeat (device token; 10 s server throttle)
// ---------------------------------------------------------------------------

// HeartbeatRequest reports liveness and what the daemon can see.
//
// PrintersDiscovered is a COUNT, not a sweep result: v1 takes a manually
// configured printer address from the server (LD-18 moved discovery to
// Phase 4), so the count is the number of printers the server itself has
// assigned to this device. Zero is a legitimate, non-crashing state and is
// exactly what makes the manager page able to say "no printer yet"
// (master-plan task 24).
type HeartbeatRequest struct {
	Version            string `json:"version"`
	OS                 string `json:"os"`
	Arch               string `json:"arch,omitempty"`
	Hostname           string `json:"hostname,omitempty"`
	PrintersDiscovered int    `json:"printersDiscovered"`
	// Discovered is the Phase-4 sweep payload. v1 always sends nil.
	Discovered []DiscoveredPrinter `json:"discovered,omitempty"`
}

// DiscoveredPrinter is the Phase-4 candidate shape, declared now so the field
// name is fixed. v1 never populates it.
type DiscoveredPrinter struct {
	Address   string `json:"address"`
	Transport string `json:"transport"`
	Identity  string `json:"identity,omitempty"` // the GS I reply, if any
}

// HeartbeatResponse returns what this device is meant to print on.
type HeartbeatResponse struct {
	Printers []Printer       `json:"printers"`
	Roles    map[string]Role `json:"roles,omitempty"` // "receipt" | "kitchen"
}

// Printer mirrors the server's DevicePrinter row.
type Printer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Transport  string `json:"transport"` // "tcp" | "usb"
	Address    string `json:"address"`   // "192.168.1.50:9100" | "/dev/usb/lp0"
	WidthDots  int    `json:"widthDots"` // 384 | 512 | 576
	DPI        int    `json:"dpi"`       // 180 | 203
	BandHeight int    `json:"bandHeight"`
	Threshold  int    `json:"threshold"`
}

// Target renders a Printer as a transport URI the transport package accepts.
func (p Printer) Target() string {
	switch p.Transport {
	case TransportUSB:
		return "usb://" + p.Address
	case TransportTCP:
		return "tcp://" + p.Address
	default:
		// A bare path or an already-schemed address (file:// in tests) passes
		// straight through; transport.Send treats a bare path as a device.
		return p.Address
	}
}

// Role mirrors the server's PrinterRole row.
type Role struct {
	PrinterID     string     `json:"printerId"`
	ExpiryMinutes int        `json:"expiryMinutes"`
	ConfirmedAt   *time.Time `json:"confirmedAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Poll — POST /api/agent/v1/jobs/poll (device token)
// ---------------------------------------------------------------------------

// PollRequest is empty today: the server derives, expires, reclaims and claims
// inside one transaction keyed on the device token (memo 5's five steps).
type PollRequest struct{}

// PollResponse carries at most one job — a receipt printer is serial anyway.
type PollResponse struct {
	Job *Job `json:"job"`
}

// Job is one claimed unit of work. Printer is nil when the server has not
// bound the job to a printer (the welcome slip before any role is assigned);
// the daemon then applies the never-blind gate of counsel finding 6.
type Job struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Printer   *Printer   `json:"printer"`
	Dots      int        `json:"dots"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Ack — POST /api/agent/v1/jobs/{id}/ack (device token, ownership-checked)
// ---------------------------------------------------------------------------

// AckRequest reports the terminal outcome.
//
// Retryable is honoured by the server ONLY for zero-byte failures; the daemon
// therefore never sets it true once BytesWritten > 0 (counsel finding 4). Both
// FailureReason and Error carry the same coarse code — see ambiguity 1 in the
// package doc.
type AckRequest struct {
	Status        string `json:"status"`
	FailureReason string `json:"failureReason,omitempty"`
	Error         string `json:"error,omitempty"`
	Retryable     bool   `json:"retryable,omitempty"`
	BytesWritten  int    `json:"bytesWritten,omitempty"`
	PrinterID     string `json:"printerId,omitempty"`
}

// AckResponse is the server's acknowledgement of the acknowledgement. The
// daemon ignores its body; a 2xx is the whole signal.
type AckResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"`
}

// Printed builds a successful ack.
func Printed(printerID string, bytesWritten int) AckRequest {
	return AckRequest{Status: StatusPrinted, PrinterID: printerID, BytesWritten: bytesWritten}
}

// Failed builds a failure ack. It enforces counsel finding 4 in code, not in a
// comment: retryable can never survive a byte reaching the printer.
func Failed(printerID, reason string, bytesWritten int, retryable bool) AckRequest {
	if bytesWritten > 0 {
		retryable = false
		if reason == "" {
			reason = FailPartial
		}
	}
	return AckRequest{
		Status:        StatusFailed,
		FailureReason: reason,
		Error:         reason,
		Retryable:     retryable,
		BytesWritten:  bytesWritten,
		PrinterID:     printerID,
	}
}
