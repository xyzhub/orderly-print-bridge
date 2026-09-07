// Package api holds EVERY request and response type the bridge exchanges with
// Orderly's `/api/agent/v1` surface, deliberately in this one file, so that
// reconciling the daemon against the merged Phase-1 handlers is a one-file
// diff rather than an archaeology exercise.
//
// # Contract source — RECONCILED against the merged Phase-1 handlers
//
// Reconciled 2026-09-07 against Orderly branch `mission/print-bridge-p1` at
// 9b6d6de3. These types now mirror SHIPPED handlers, not a written sketch:
//
//   - server/api/agent/v1/enroll.post.ts          + server/utils/print/enrollment.ts
//   - server/api/agent/v1/heartbeat.post.ts       + server/utils/print/heartbeat.ts
//   - server/api/agent/v1/jobs/poll.post.ts       + server/utils/print/poll.ts
//   - server/api/agent/v1/jobs/[id]/ack.post.ts   + server/utils/print/ack.ts
//   - server/api/agent/v1/jobs/[id]/artifact.get.ts
//   - server/utils/print/device-token.ts          (the `odb_` token shape)
//   - server/utils/db/queries/print.ts            (PRINT_FAILURE_REASONS)
//
// The design reasons behind the shapes remain: memo 4 + the endpoint table in
// docs/product/decisions/2026-09-07-print-bridge-memos.md, and counsel
// findings 4 (any byte written ⇒ terminal `failed{partial}`), 6 (never
// blind-print) and 9 (`expired` is not a `conflict`).
//
// # What the reconciliation changed
//
//  1. The ack sends `{failureReason, lastError}`. The pre-merge build also
//     sent `error`; the server accepts it only as a deprecated one-release
//     alias of `lastError`, so it is GONE from this build. `failureReason`
//     must be one of the server's six coarse tokens (anything else is stored
//     as `unknown`); the daemon's richer word survives in `lastError`.
//  2. `retryable` and `bytesWritten` are ADVISORY. `ack.ts` re-derives the
//     partial-write decision from `bytesWritten` itself rather than trusting
//     a flag from a daemon build it does not control — the daemon still sets
//     them honestly, but the server is the authority.
//  3. Error bodies are h3's `{statusCode, statusMessage, data:{error}}` and
//     the MACHINE CODE is in both `statusMessage` and `data.error`. HTTP
//     status stays the primary discriminator, but 409 carries TWO codes
//     (`already_claimed` and `serial_conflict`), so the code is read too.
//  4. A 410 on the artifact is acked `voided`, not `failed` — the handler's
//     own comment names that as the expected daemon behaviour, and the job is
//     already terminal server-side.
//  5. `PollResponse.job.printer` is non-null whenever a job is returned
//     (`poll.ts` claims only when the role's printer belongs to this device).
//     The welcome/test candidate gate stays as defence, not as a normal path.
//
// # Still open
//
//   - `welcome` / `test` artifacts currently answer 501
//     `kind_not_renderable_yet` until S5's slip routes merge; the daemon
//     treats their PNGs like any other once they land.
package api

import (
	"regexp"
	"time"
)

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

// FailureReasons are the SIX coarse tokens the server stores in
// `PrintJob.failureReason` (`PRINT_FAILURE_REASONS` in
// server/utils/db/queries/print.ts). They are the only strings the manager
// surface maps to human words, so a seventh would render as itself; `ack.ts`
// silently rewrites anything else to `unknown`. The daemon's own richer word
// travels in `lastError`, where a human reads it.
const (
	// FailPartial — bytes reached the printer before the write failed.
	// TERMINAL by contract (counsel finding 4): never retryable, because a
	// retry after a partial write is how a venue gets two legal invoices.
	// The server re-derives this from bytesWritten regardless of what we send.
	FailPartial = "partial"
	// FailRenderFailed — the artifact was unusable at this printer's width,
	// or the render app could not produce it. Never resample.
	FailRenderFailed = "render_failed"
	// FailOffline — zero bytes written; the printer never answered, refused
	// the connection, or is out of paper. The only class the bridge marks
	// retryable. (The pre-merge build called this `unreachable`, which the
	// server aliases here; we now send the stored token directly.)
	FailOffline = "offline"
	// FailTimeout — the job did not finish inside the 60 s ack window. Bytes
	// written is unknown, so this is terminal, not retryable.
	FailTimeout = "timeout"
	// FailNoPrinter — no printer is assigned, or (for a welcome slip) no
	// candidate could be identified safely. Terminal; the manager re-enqueues.
	FailNoPrinter = "no_printer"
	// FailUnknown — the catch-all. `artifact_gone` used to be sent here; a
	// 410 is now acked as `voided` instead (see the package doc, item 4).
	FailUnknown = "unknown"
)

// FailureReasons is the closed set the server stores. Anything outside it is
// rewritten to FailUnknown server-side.
var FailureReasons = []string{FailTimeout, FailOffline, FailPartial, FailNoPrinter, FailRenderFailed, FailUnknown}

// Machine codes carried in `statusMessage` and `data.error`. The HTTP status
// is the primary discriminator; these split the two 409s and name the rest.
const (
	CodeInvalidCode    = "invalid_code"            // 400
	CodeUnknownCode    = "unknown_code"            // 404
	CodeCodeExpired    = "code_expired"            // 410
	CodeAlreadyClaimed = "already_claimed"         // 409
	CodeSerialConflict = "serial_conflict"         // 409
	CodeDeviceRevoked  = "device_revoked"          // 403
	CodeJobNotOwned    = "job_not_owned"           // 403 on ack/artifact
	CodeLeaseLost      = "lease_lost"              // 409 on artifact — re-queued
	CodeArtifactGone   = "artifact_gone"           // 410 on artifact
	CodeNotRenderable  = "kind_not_renderable_yet" // 501 on artifact (S5 seam)
	CodeRenderFailed   = "render_failed"           // 502 on artifact
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

// DeviceTokenPattern mirrors server/utils/print/device-token.ts: `odb_` plus
// the base64url of 32 random bytes (43 characters). Validating it on receipt
// turns a proxy that rewrote the body into an immediate, named failure rather
// than a device that 401s forever.
var DeviceTokenPattern = regexp.MustCompile(`^odb_[A-Za-z0-9_-]{43}$`)

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
// FailureReason is one of the six coarse tokens above; LastError is the raw
// driver string a human reads (truncated to 300 chars server-side). The
// deprecated `error` alias is deliberately NOT sent — this build is the one
// that lets the server drop it.
//
// Retryable is honoured by the server ONLY for zero-byte failures; the daemon
// never sets it true once BytesWritten > 0 (counsel finding 4), and `ack.ts`
// re-derives the same decision independently.
type AckRequest struct {
	Status        string `json:"status"`
	FailureReason string `json:"failureReason,omitempty"`
	LastError     string `json:"lastError,omitempty"`
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

// Voided builds the ack for a job the server has already terminated — the 410
// on the artifact (voided by a browser print, or expired). The handler's own
// comment names `voided` as the expected daemon answer.
func Voided(printerID, detail string) AckRequest {
	return AckRequest{Status: StatusVoided, PrinterID: printerID, LastError: detail}
}

// Failed builds a failure ack. It enforces counsel finding 4 in code, not in a
// comment: retryable can never survive a byte reaching the printer. `detail`
// is the raw driver string for a human; `reason` is coerced into the server's
// closed token set so nothing silently lands on `unknown`.
func Failed(printerID, reason, detail string, bytesWritten int, retryable bool) AckRequest {
	if bytesWritten > 0 {
		retryable = false
		if reason == "" {
			reason = FailPartial
		}
	}
	return AckRequest{
		Status:        StatusFailed,
		FailureReason: coarse(reason),
		LastError:     detail,
		Retryable:     retryable,
		BytesWritten:  bytesWritten,
		PrinterID:     printerID,
	}
}

// coarse keeps the daemon inside the server's stored vocabulary.
func coarse(reason string) string {
	for _, known := range FailureReasons {
		if reason == known {
			return reason
		}
	}
	return FailUnknown
}
