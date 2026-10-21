package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/availability"
)

var limitsNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestBar(t *testing.T) {
	cases := map[float64]string{0: "░░░░░░░░░░", 24: "▓▓░░░░░░░░", 25: "▓▓▓░░░░░░░", 100: "▓▓▓▓▓▓▓▓▓▓", 140: "▓▓▓▓▓▓▓▓▓▓", -5: "░░░░░░░░░░"}
	for in, want := range cases {
		if got := bar(in); got != want {
			t.Errorf("bar(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestLimitsSummary(t *testing.T) {
	u := availability.UsageSeen{SeenAt: limitsNow.Add(-12 * time.Minute), Windows: []availability.Window{
		{Kind: "five_hour", PercentUsed: 80, ResetsAt: limitsNow.Add(40 * time.Minute)},
		{Kind: "seven_day", PercentUsed: 12, ResetsAt: limitsNow.Add(3 * 24 * time.Hour)},
	}}
	got := limitsSummary(u, limitsNow)
	for _, want := range []string{"5h ▓▓▓▓▓▓▓▓░░  80% (resets in 41m)", "7d ▓░░░░░░░░░  12% (resets in 3d)", "seen 12m ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestLimitsSummaryDropsWindowsThatHaveReset(t *testing.T) {
	u := availability.UsageSeen{SeenAt: limitsNow.Add(-3 * time.Hour), Windows: []availability.Window{
		{Kind: "five_hour", PercentUsed: 99, ResetsAt: limitsNow.Add(-time.Minute)},
		{Kind: "seven_day", PercentUsed: 40, ResetsAt: limitsNow.Add(24 * time.Hour)},
	}}
	got := limitsSummary(u, limitsNow)
	if strings.Contains(got, "5h") || !strings.Contains(got, "7d") {
		t.Errorf("got %q", got)
	}
	all := availability.UsageSeen{SeenAt: limitsNow.Add(-9 * time.Hour), Windows: u.Windows[:1]}
	if got := limitsSummary(all, limitsNow); !strings.Contains(got, "reset since") {
		t.Errorf("got %q", got)
	}
}

func TestAccountsListShowsLimits(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a", "b")
	h.writeState(t, "state.json", `{"version":1,"usage":{"a":{"seen_at":"2026-10-20T11:48:00Z","windows":[{"kind":"five_hour","percent_used":80,"resets_at":"2026-10-20T12:40:00Z"}]}}}`)
	h.run("accounts", "list")
	out := h.stdout.String()
	if !strings.Contains(out, "5h ▓▓▓▓▓▓▓▓░░  80% (resets in 41m)") || !strings.Contains(out, "no reading yet") {
		t.Errorf("out:\n%s", out)
	}
}
