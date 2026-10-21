// Package limits classifies Claude Code failure reports and extracts reset times.
//
// The StopFailure payload is only partly documented, so parsing is lenient and
// every decision that depends on free text is conservative: only an explicit
// signal moves an account out of rotation, and a bare rate-limit error without
// any text is treated as a usage cap with an estimated reset.
package limits

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is what a failure means for account selection.
type Kind string

const (
	// Ignore: not about the account (bad request, output truncated, unknown model).
	Ignore Kind = "ignore"
	// Transient: overload or a retryable rate limit; the same account should be retried.
	Transient Kind = "transient"
	// AccountLimit: the account's usage window is spent.
	AccountLimit Kind = "account_limit"
	// ModelLimit: a single model's allowance is spent; the account still works for others.
	ModelLimit Kind = "model_limit"
	// Auth: the login is invalid or expired and needs the user to sign in again.
	Auth Kind = "auth"
	// Blocked: billing problem or account hold; only the user can fix it.
	Blocked Kind = "blocked"
)

// DefaultCooldown is assumed when a limit carries no parseable reset time:
// Claude's short usage window is five hours.
const DefaultCooldown = 5 * time.Hour

// maxPlausibleReset rejects reset times too far away to be a usage window.
const maxPlausibleReset = 8 * 24 * time.Hour

// Failure is a classified StopFailure.
type Failure struct {
	Kind      Kind      `json:"kind"`
	Error     string    `json:"error"`
	Message   string    `json:"message,omitempty"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Estimated bool      `json:"estimated,omitempty"`
}

type rawFailure struct {
	Error                json.RawMessage `json:"error"`
	ErrorDetails         json.RawMessage `json:"error_details"`
	Message              string          `json:"message"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// text flattens a JSON string, or an object's message/type fields, into plain text.
func text(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Type + " " + obj.Message)
	}
	return string(raw)
}

// errorCode returns the machine error value from a string or {"type": ...} object.
func errorCode(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Type)
	}
	return ""
}

var (
	modelLimitRe  = regexp.MustCompile(`(?i)\b(opus|sonnet|haiku|fable)\b[^.\n]{0,40}\blimit`)
	usageLimitRe  = regexp.MustCompile(`(?i)(usage limit|limit reached|hit your limit|\bresets?\b|weekly limit|5-hour limit)`)
	transientRe   = regexp.MustCompile(`(?i)(too many requests|try again|retry|temporar|slow down)`)
	resetClockRe  = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?(?:([A-Za-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b(?:\s*\(([A-Za-z_]+(?:/[A-Za-z_+\-0-9]+)*)\))?`)
	resetRelative = regexp.MustCompile(`(?i)\bresets?\s+in\s+(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m)\b`)
)

// Classify interprets a StopFailure hook payload. now anchors relative reset
// times; local is used when the message names no time zone.
func Classify(payload []byte, now time.Time, local *time.Location) (Failure, error) {
	var raw rawFailure
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Failure{}, fmt.Errorf("parse failure payload: %w", err)
	}
	code := errorCode(raw.Error)
	msg := strings.TrimSpace(strings.Join(nonEmpty(text(raw.ErrorDetails), raw.Message, raw.LastAssistantMessage), " "))
	f := Failure{Error: code, Message: msg}

	switch code {
	case "rate_limit", "rate_limit_error":
		switch {
		case modelLimitRe.MatchString(msg):
			f.Kind = ModelLimit
		case msg != "" && !usageLimitRe.MatchString(msg) && transientRe.MatchString(msg):
			f.Kind = Transient
			return f, nil
		default:
			f.Kind = AccountLimit
		}
		f.ResetAt, f.Estimated = resetTime(msg, now, local)
	case "overloaded", "overloaded_error", "server_error", "unknown":
		f.Kind = Transient
	case "authentication_failed", "oauth_org_not_allowed":
		f.Kind = Auth
	case "account_on_hold", "billing_error":
		f.Kind = Blocked
	default:
		f.Kind = Ignore
	}
	return f, nil
}

func resetTime(msg string, now time.Time, local *time.Location) (at time.Time, estimated bool) {
	if t, ok := ParseReset(msg, now, local); ok {
		return t, false
	}
	return now.Add(DefaultCooldown), true
}

// ParseReset extracts "resets 3pm (America/New_York)", "resets Oct 24, 2pm" or
// "resets in 2 hours" from a limit message. Implausible results are rejected.
func ParseReset(msg string, now time.Time, local *time.Location) (time.Time, bool) {
	if local == nil {
		local = time.Local
	}
	if m := resetRelative.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := strings.ToLower(m[2])
		var d time.Duration
		switch {
		case strings.HasPrefix(unit, "d"):
			d = time.Duration(n) * 24 * time.Hour
		case strings.HasPrefix(unit, "h"):
			d = time.Duration(n) * time.Hour
		default:
			d = time.Duration(n) * time.Minute
		}
		return plausible(now.Add(d), now)
	}
	m := resetClockRe.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	loc := local
	if m[6] != "" {
		l, err := time.LoadLocation(m[6])
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	if hour < 1 || hour > 12 || minute > 59 {
		return time.Time{}, false
	}
	hour %= 12
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}

	nowIn := now.In(loc)
	if m[1] != "" {
		month, ok := parseMonth(m[1])
		if !ok {
			return time.Time{}, false
		}
		day, _ := strconv.Atoi(m[2])
		t := time.Date(nowIn.Year(), month, day, hour, minute, 0, 0, loc)
		if !t.After(now) {
			t = t.AddDate(1, 0, 0)
		}
		return plausible(t, now)
	}
	t := time.Date(nowIn.Year(), nowIn.Month(), nowIn.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return plausible(t, now)
}

func plausible(t, now time.Time) (time.Time, bool) {
	if !t.After(now) || t.Sub(now) > maxPlausibleReset {
		return time.Time{}, false
	}
	return t, true
}

func parseMonth(s string) (time.Month, bool) {
	s = strings.ToLower(s)
	for m := time.January; m <= time.December; m++ {
		name := strings.ToLower(m.String())
		if s == name || (len(s) >= 3 && strings.HasPrefix(name, s)) {
			return m, true
		}
	}
	return 0, false
}

func nonEmpty(in ...string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
