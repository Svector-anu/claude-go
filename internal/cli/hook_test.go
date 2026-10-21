package cli

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Svector-anu/claude-go/internal/control"
	"github.com/Svector-anu/claude-go/internal/limits"
)

func (h *harness) hook(t *testing.T, wrapped, payload string, args ...string) int {
	t.Helper()
	env := h.env
	env.Stdin = strings.NewReader(payload)
	env.Getenv = func(k string) string {
		if k == "CLAUDE_GO_WRAPPED" {
			return wrapped
		}
		return h.vars[k]
	}
	if len(args) == 0 {
		args = []string{"stop-failure"}
	}
	return Run(t.Context(), append([]string{"hook"}, args...), env)
}

func (h *harness) pendingEvent() *control.Event {
	ev, _ := control.Dir(h.home + "/runtime").TakeEvent(os.Getpid())
	return ev
}

func TestHookReportsUsageLimitToWrapper(t *testing.T) {
	h := newHarness(t)
	payload := `{"session_id":"s-1","hook_event_name":"StopFailure","error":"rate_limit","error_details":"5-hour limit reached ∙ resets 3pm (America/New_York)"}`
	if code := h.hook(t, itoa(os.Getpid()), payload); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	ev := h.pendingEvent()
	if ev == nil || ev.Failure.Kind != limits.AccountLimit || ev.SessionID != "s-1" || ev.Failure.ResetAt.IsZero() {
		t.Fatalf("event = %+v", ev)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("hook wrote to stdout: %q", h.stdout.String())
	}
}

func TestHookIgnoresNonAccountFailures(t *testing.T) {
	h := newHarness(t)
	for _, payload := range []string{
		`{"error":"overloaded"}`, `{"error":"server_error"}`, `{"error":"invalid_request"}`,
		`{"error":"rate_limit","error_details":"Too many requests, please try again"}`,
	} {
		if code := h.hook(t, itoa(os.Getpid()), payload); code != exitOK {
			t.Errorf("code = %d", code)
		}
		if ev := h.pendingEvent(); ev != nil {
			t.Errorf("%s produced an event: %+v", payload, ev)
		}
	}
}

func TestHookDoesNothingOutsideAWrappedSession(t *testing.T) {
	h := newHarness(t)
	if code := h.hook(t, "", `{"error":"rate_limit"}`); code != exitOK {
		t.Errorf("code = %d", code)
	}
	if ev := h.pendingEvent(); ev != nil {
		t.Errorf("plain claude must be unaffected, got %+v", ev)
	}
	if _, err := os.Stat(h.home + "/runtime"); err == nil {
		t.Error("hook created state outside a wrapped session")
	}
}

func TestHookNeverFailsClaude(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ wrapped, payload string }{
		{itoa(os.Getpid()), "not json"},
		{"banana", `{"error":"rate_limit"}`},
		{"-4", `{"error":"rate_limit"}`},
	} {
		if code := h.hook(t, c.wrapped, c.payload); code != exitOK {
			t.Errorf("%+v: code = %d", c, code)
		}
	}
}

func TestHookRejectsUnknownSubcommand(t *testing.T) {
	h := newHarness(t)
	if code := h.hook(t, "1", "{}", "nope"); code != exitUsage {
		t.Errorf("code = %d", code)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
