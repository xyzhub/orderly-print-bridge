package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xyz/orderly-print-bridge/internal/secret"
	"github.com/xyz/orderly-print-bridge/internal/version"
)

// MaxArtifactBytes bounds an artifact read so a hostile or broken response
// cannot exhaust a 2 GB thin client's memory. A 576×2000 1-bit receipt PNG is
// well under 1 MB; 16 MB is three orders of headroom.
const MaxArtifactBytes = 16 << 20

// Client speaks the /api/agent/v1 contract. The zero value is not usable —
// use New.
type Client struct {
	BaseURL string
	// Token is the device bearer token. It is redacted by every formatting
	// and marshalling path; only the Authorization header reveals it.
	Token secret.Secret
	HTTP  *http.Client
}

// New returns a Client with sane timeouts. An empty token is legal: the
// enroll call is unauthenticated.
func New(baseURL string, token secret.Secret) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Error is a non-2xx response from Orderly. Status is the authoritative
// discriminator (410 code_expired vs 409 already_claimed — counsel finding 9);
// Code is the server's machine string when it sends one.
type Error struct {
	Status  int
	Code    string
	Message string
	Path    string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s: %d %s (%s)", e.Path, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("%s: %d %s", e.Path, e.Status, msg)
}

// IsStatus reports whether err is an *Error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *Error
	if !asError(err, &apiErr) {
		return false
	}
	return apiErr.Status == status
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Enroll claims a setup code. On 410 the code expired; on 409 it was already
// claimed. Callers MUST report those two differently — the same message for
// both is what made a box "sit silent four ways" (counsel finding 9).
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	if req.Version == "" {
		req.Version = version.Version
	}
	var out EnrollResponse
	if err := c.do(ctx, http.MethodPost, PathEnroll, req, &out, false); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, fmt.Errorf("%s: server returned no device token", PathEnroll)
	}
	return &out, nil
}

// Heartbeat reports liveness and returns the printers assigned to this device.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var out HeartbeatResponse
	if err := c.do(ctx, http.MethodPost, PathHeartbeat, req, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// Poll asks for at most one claimed job.
func (c *Client) Poll(ctx context.Context) (*PollResponse, error) {
	var out PollResponse
	if err := c.do(ctx, http.MethodPost, PathPoll, PollRequest{}, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// Artifact fetches the rendered PNG for a claimed job. A 410 means the job was
// voided or expired while in flight.
func (c *Client) Artifact(ctx context.Context, jobID string) ([]byte, error) {
	path := fmt.Sprintf(PathArtifact, jobID)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readAPIError(resp, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read artifact: %w", path, err)
	}
	if len(body) > MaxArtifactBytes {
		return nil, fmt.Errorf("%s: artifact exceeds %d bytes", path, MaxArtifactBytes)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%s: artifact is empty", path)
	}
	return body, nil
}

// Ack reports the terminal outcome of a job.
func (c *Client) Ack(ctx context.Context, jobID string, req AckRequest) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf(PathAck, jobID), req, nil, true)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any, auth bool) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, image/png")
	req.Header.Set("User-Agent", version.UserAgent)
	if auth {
		if c.Token.IsZero() {
			return nil, fmt.Errorf("%s: no device token (not enrolled)", path)
		}
		// The one deliberate disclosure of the token in the whole daemon.
		req.Header.Set("Authorization", "Bearer "+c.Token.Reveal())
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, auth bool) error {
	req, err := c.newRequest(ctx, method, path, body, auth)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readAPIError(resp, path)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", path, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", path, err)
	}
	return nil
}

// errorBody is the tolerant read of an H3 error payload. Nitro sends
// {statusCode, statusMessage, message, data}; some handlers put a machine code
// in data.code, others at the top level. The HTTP status stays authoritative.
type errorBody struct {
	StatusCode    int    `json:"statusCode"`
	StatusMessage string `json:"statusMessage"`
	Message       string `json:"message"`
	Code          string `json:"code"`
	Data          struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"data"`
}

func readAPIError(resp *http.Response, path string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	apiErr := &Error{Status: resp.StatusCode, Path: path}
	var parsed errorBody
	if json.Unmarshal(raw, &parsed) == nil {
		apiErr.Code = firstNonEmpty(parsed.Data.Code, parsed.Code)
		apiErr.Message = firstNonEmpty(parsed.Data.Message, parsed.StatusMessage, parsed.Message)
	}
	if apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(raw))
		if len(apiErr.Message) > 200 {
			apiErr.Message = apiErr.Message[:200]
		}
	}
	return apiErr
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
