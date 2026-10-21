package statusline

import (
	"strings"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/ipc"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func status(label, attention, phase string) *ipc.Status {
	return &ipc.Status{Session: ipc.SessionView{Account: ipc.AccountRef{ID: "a", Label: label}, Attention: attention, Phase: phase}}
}

func TestParseUsesOnlyDocumentedFields(t *testing.T) {
	in := Parse([]byte(`{"session_id":"s","model":{"id":"x"},"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1738425600},"seven_day":{"used_percentage":41.2,"resets_at":1738857600}},"extra":{"a":1}}`))
	if in.SessionID != "s" || in.RateLimits.FiveHour.UsedPercentage != 23.5 {
		t.Fatalf("in = %+v", in)
	}
	w := in.Windows()
	if len(w) != 2 || w[0].Kind != "five_hour" || w[0].ResetsAt.Unix() != 1738425600 || w[1].Kind != "seven_day" {
		t.Errorf("windows = %+v", w)
	}
}

func TestParseToleratesGarbageAndAbsentRateLimits(t *testing.T) {
	for _, data := range []string{"", "not json", "[]", `{"rate_limits":null}`, `{}`} {
		in := Parse([]byte(data))
		if len(in.Windows()) != 0 {
			t.Errorf("%q produced windows", data)
		}
	}
	// A window may be absent on its own.
	in := Parse([]byte(`{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":0}}}`))
	if w := in.Windows(); len(w) != 1 || w[0].Kind != "seven_day" || w[0].ResetsAt != nil {
		t.Errorf("windows = %+v", w)
	}
}

func TestRender(t *testing.T) {
	soon := now.Add(90 * time.Minute)
	in := Parse([]byte(`{"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1},"seven_day":{"used_percentage":93,"resets_at":1}}}`))
	cases := []struct {
		name     string
		in       Input
		st       *ipc.Status
		accounts []ipc.AccountView
		want     string
	}{
		{"unreachable", in, nil, nil, "claude-go: controller unreachable"},
		{"no usage yet", Input{}, status("Work", "", "running"), nil, "⇄ Work"},
		{"usage and a ready account", in, status("Work", "", "running"),
			[]ipc.AccountView{{ID: "a", Current: true}, {ID: "b", State: ipc.StateReady}}, "⇄ Work · 5h 24% · 7d 93%! · 1 other ready"},
		{"others waiting", Input{}, status("Work", "", "running"),
			[]ipc.AccountView{{ID: "a", Current: true}, {ID: "b", State: ipc.StateLimit, Until: &soon}}, "⇄ Work · others back in 1h30m"},
		{"others needing login", Input{}, status("Work", "", "running"),
			[]ipc.AccountView{{ID: "a", Current: true}, {ID: "b", State: ipc.StateNeedsLogin}}, "⇄ Work · no other account ready"},
		{"only account", Input{}, status("Work", "", "running"), []ipc.AccountView{{ID: "a", Current: true}}, "⇄ Work"},
		{"attention", Input{}, status("Work", "no other account", "running"), nil, "⇄ Work · ⚠ needs attention"},
		{"switching", Input{}, status("Work", "", "handing_off"), nil, "⇄ Work · switching…"},
		{"long label", Input{}, status(strings.Repeat("x", 40), "", "running"), nil, "⇄ " + strings.Repeat("x", 23) + "…"},
	}
	for _, c := range cases {
		if got := Render(c.in, c.st, c.accounts, now); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestRenderNeverContainsCredentialLikeFields(t *testing.T) {
	out := Render(Input{}, status("Work", "", "running"), []ipc.AccountView{{ID: "a", Current: true, Label: "Work"}}, now)
	for _, bad := range []string{"token", "secret", "sk-ant"} {
		if strings.Contains(strings.ToLower(out), bad) {
			t.Errorf("output mentions %q", bad)
		}
	}
}
