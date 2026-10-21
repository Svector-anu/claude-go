// Package ipc is the small, versioned interface between the Go controller and
// Claude-side extensions (the claude-go mod, status line, diagnostics).
//
// Transport: HTTP/1.1 with JSON bodies over a Unix domain socket that only the
// current user can open (socket 0600 inside a 0700 directory). Paths are
// versioned (/v1/...). No credentials are ever part of a request or response.
package ipc

import "time"

// APIVersion is the version of the /v1 interface.
const APIVersion = 1

// Error codes returned in ErrorBody.Code.
const (
	CodeInvalid  = "invalid_request"
	CodeRejected = "rejected"
	CodeBusy     = "busy"
	CodeTooLarge = "too_large"
	CodeMethod   = "method_not_allowed"
	CodeNotFound = "not_found"
	CodeTimeout  = "timeout"
	CodeInternal = "internal"
)

// ErrorBody is the JSON body of every non-2xx response.
type ErrorBody struct {
	Error   ErrorDetail `json:"error"`
	Version int         `json:"api_version"`
}

// ErrorDetail describes a failure.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID names the handoff that blocks this one, for CodeBusy.
	RequestID string `json:"request_id,omitempty"`
}

// Health answers GET /v1/health.
type Health struct {
	OK         bool   `json:"ok"`
	APIVersion int    `json:"api_version"`
	PID        int    `json:"pid"`
	Version    string `json:"version"`
}

// AccountRef names an account without exposing anything sensitive.
type AccountRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SessionView is the supervised session.
type SessionView struct {
	ID               string     `json:"id,omitempty"`
	Cwd              string     `json:"cwd,omitempty"`
	Account          AccountRef `json:"account"`
	Phase            string     `json:"phase"`
	HandoffAvailable bool       `json:"handoff_available"`
	HandoffBlock     string     `json:"handoff_block,omitempty"`
	// Attention is set when the user may need to act; Action says what to do.
	Attention string `json:"attention,omitempty"`
	Action    string `json:"action,omitempty"`
	Handoffs  int    `json:"handoffs"`
}

// HistoryEntry mirrors one recorded handoff outcome.
type HistoryEntry struct {
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Trigger   string    `json:"trigger"`
	Reason    string    `json:"reason,omitempty"`
	Result    string    `json:"result"`
	Message   string    `json:"message,omitempty"`
}

// Status answers GET /v1/status.
type Status struct {
	APIVersion  int           `json:"api_version"`
	PID         int           `json:"pid"`
	Version     string        `json:"version"`
	StartedAt   time.Time     `json:"started_at"`
	Session     SessionView   `json:"session"`
	LastHandoff *HistoryEntry `json:"last_handoff,omitempty"`
}

// Window is one rate-limit window as Claude reported it.
type Window struct {
	Kind        string     `json:"kind"`
	PercentUsed float64    `json:"percent_used"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
}

// UsageSeen is the last usage reading for an account and when it was taken.
// It is only ever a reading of that moment, never a live value.
type UsageSeen struct {
	SeenAt  time.Time `json:"seen_at"`
	Windows []Window  `json:"windows"`
}

// Account states shown to the user.
const (
	StateReady      = "ready"
	StateLimit      = "limit"
	StateModelLimit = "model_limit"
	StateNeedsLogin = "needs_signin"
	StateBlocked    = "blocked"
)

// AccountView is one registered account and whether it can be used.
type AccountView struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Default   bool       `json:"default,omitempty"`
	Current   bool       `json:"current,omitempty"`
	State     string     `json:"state"`
	Until     *time.Time `json:"until,omitempty"`
	Estimated bool       `json:"estimated,omitempty"`
	LastUsage *UsageSeen `json:"last_usage,omitempty"`
}

// AccountsResponse answers GET /v1/accounts.
type AccountsResponse struct {
	Accounts []AccountView `json:"accounts"`
}

// HistoryResponse answers GET /v1/history.
type HistoryResponse struct {
	Entries []HistoryEntry `json:"entries"`
}

// HandoffRequest is the body of POST /v1/handoff.
type HandoffRequest struct {
	RequestID string `json:"request_id"`
	To        string `json:"to"`
}

// HandoffAccepted answers a handoff that passed validation. The running Claude
// process is stopped right after this response, so the final outcome is found
// in the history and shown by the replacement process.
type HandoffAccepted struct {
	RequestID string `json:"request_id"`
	Accepted  bool   `json:"accepted"`
	To        string `json:"to"`
	Replayed  bool   `json:"replayed,omitempty"`
}

// UsageReport is the body of POST /v1/usage: the rate-limit windows of the
// account the session is currently running on, as Claude reported them.
type UsageReport struct {
	Windows []Window `json:"windows"`
}

// OK answers requests that carry no data.
type OK struct {
	OK bool `json:"ok"`
}
