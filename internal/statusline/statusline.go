// Package statusline renders claude-go's segment for Claude Code's status line.
//
// Input is the documented statusLine stdin JSON. Only fields that are
// documented to exist are read; rate_limits describes the session's own
// account, so nothing here claims to know another account's usage.
package statusline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"claude-go/internal/ipc"
)

// Input is the subset of the statusLine stdin JSON claude-go reads.
type Input struct {
	SessionID  string `json:"session_id"`
	RateLimits *struct {
		FiveHour   *Window `json:"five_hour"`
		SevenDay   *Window `json:"seven_day"`
		SpendLimit *Window `json:"spend_limit"`
	} `json:"rate_limits"`
}

// Window is one documented rate-limit window.
type Window struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       float64 `json:"resets_at"` // Unix epoch seconds
}

// Parse reads the stdin JSON. Anything unparseable yields an empty Input: a
// status line must never fail because of its input.
func Parse(data []byte) Input {
	var in Input
	_ = json.Unmarshal(data, &in)
	return in
}

// Windows converts the documented windows into IPC usage windows.
func (in Input) Windows() []ipc.Window {
	if in.RateLimits == nil {
		return nil
	}
	var out []ipc.Window
	add := func(kind string, w *Window) {
		if w == nil {
			return
		}
		win := ipc.Window{Kind: kind, PercentUsed: w.UsedPercentage}
		if w.ResetsAt > 0 {
			t := time.Unix(int64(w.ResetsAt), 0).UTC()
			win.ResetsAt = &t
		}
		out = append(out, win)
	}
	add("five_hour", in.RateLimits.FiveHour)
	add("seven_day", in.RateLimits.SevenDay)
	add("spend_limit", in.RateLimits.SpendLimit)
	return out
}

const maxLabel = 24

func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxLabel {
		return s
	}
	r := []rune(s)
	return string(r[:maxLabel-1]) + "…"
}

func windowText(prefix string, w *Window) string {
	if w == nil {
		return ""
	}
	s := fmt.Sprintf("%s %.0f%%", prefix, w.UsedPercentage)
	if w.UsedPercentage >= 90 {
		s += "!"
	}
	return s
}

// Render builds the segment. st and accounts may be nil when the controller
// could not be reached; the segment then says so instead of guessing.
func Render(in Input, st *ipc.Status, accounts []ipc.AccountView, now time.Time) string {
	if st == nil {
		return "claude-go: controller unreachable"
	}
	parts := []string{"⇄ " + clip(st.Session.Account.Label)}
	if in.RateLimits != nil {
		if t := windowText("5h", in.RateLimits.FiveHour); t != "" {
			parts = append(parts, t)
		}
		if t := windowText("7d", in.RateLimits.SevenDay); t != "" {
			parts = append(parts, t)
		}
		if t := windowText("spend", in.RateLimits.SpendLimit); t != "" {
			parts = append(parts, t)
		}
	}
	if accounts != nil {
		others, ready := 0, 0
		var nextReset *time.Time
		for _, a := range accounts {
			if a.Current {
				continue
			}
			others++
			if a.State == ipc.StateReady || a.State == ipc.StateModelLimit {
				ready++
			} else if a.Until != nil && (nextReset == nil || a.Until.Before(*nextReset)) {
				nextReset = a.Until
			}
		}
		switch {
		case others == 0:
		case ready > 0:
			parts = append(parts, fmt.Sprintf("%d other ready", ready))
		case nextReset != nil:
			parts = append(parts, "others back in "+until(*nextReset, now))
		default:
			parts = append(parts, "no other account ready")
		}
	}
	if st.Session.Attention != "" {
		parts = append(parts, "⚠ needs attention")
	} else if st.Session.Phase == "handing_off" {
		parts = append(parts, "switching…")
	}
	return strings.Join(parts, " · ")
}

func until(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
