package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/Svector-anu/claude-go/internal/availability"
)

var windowNames = map[string]string{"five_hour": "5h", "seven_day": "7d", "spend_limit": "spend"}

func bar(pct float64) string {
	n := int(pct/10 + 0.5)
	n = max(0, min(10, n))
	return strings.Repeat("▓", n) + strings.Repeat("░", 10-n)
}

func humanSince(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanUntil(t, now time.Time) string {
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

// limitsSummary renders an account's last-seen usage on one line. Windows whose
// reset time has passed are dropped: that period is over, so their reading is stale.
func limitsSummary(u availability.UsageSeen, now time.Time) string {
	var parts []string
	for _, w := range u.Windows {
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
			continue
		}
		name := windowNames[w.Kind]
		if name == "" {
			name = w.Kind
		}
		p := fmt.Sprintf("%s %s %3.0f%%", name, bar(w.PercentUsed), w.PercentUsed)
		if !w.ResetsAt.IsZero() {
			p += " (resets in " + humanUntil(w.ResetsAt, now) + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "usage has reset since it was last seen"
	}
	return strings.Join(parts, "  ") + "  · seen " + humanSince(u.SeenAt, now)
}
