package limits

import (
	"testing"
	"time"
)

var (
	ny   = mustLoc("America/New_York")
	now  = time.Date(2026, 10, 20, 12, 0, 0, 0, ny) // noon in New York
	utcF = time.UTC
)

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    Kind
	}{
		{"usage cap with reset", `{"error":"rate_limit","error_details":"5-hour limit reached ∙ resets 3pm (America/New_York)"}`, AccountLimit},
		{"weekly cap", `{"error":"rate_limit","last_assistant_message":"You've hit your weekly limit · resets Oct 24, 2pm"}`, AccountLimit},
		{"model specific", `{"error":"rate_limit","error_details":"Opus weekly limit reached ∙ resets Oct 24, 2pm"}`, ModelLimit},
		{"object-shaped error", `{"error":{"type":"rate_limit_error","message":"Usage limit reached, resets 3pm"}}`, AccountLimit},
		{"bare rate_limit, no text", `{"error":"rate_limit"}`, AccountLimit},
		{"retryable 429 wording", `{"error":"rate_limit","error_details":"Too many requests, please try again"}`, Transient},
		{"overloaded", `{"error":"overloaded"}`, Transient},
		{"server error", `{"error":"server_error","error_details":"500"}`, Transient},
		{"auth failed", `{"error":"authentication_failed"}`, Auth},
		{"org not allowed", `{"error":"oauth_org_not_allowed"}`, Auth},
		{"billing", `{"error":"billing_error"}`, Blocked},
		{"on hold", `{"error":"account_on_hold"}`, Blocked},
		{"invalid request", `{"error":"invalid_request"}`, Ignore},
		{"max output", `{"error":"max_output_tokens"}`, Ignore},
		{"model not found", `{"error":"model_not_found"}`, Ignore},
		{"missing error", `{}`, Ignore},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := Classify([]byte(c.payload), now, utcF)
			if err != nil {
				t.Fatal(err)
			}
			if f.Kind != c.want {
				t.Errorf("Kind = %s, want %s (%+v)", f.Kind, c.want, f)
			}
		})
	}
}

func TestClassifyRejectsGarbage(t *testing.T) {
	if _, err := Classify([]byte("not json"), now, utcF); err == nil {
		t.Fatal("expected error")
	}
}

func TestClassifyResetHandling(t *testing.T) {
	f, _ := Classify([]byte(`{"error":"rate_limit","error_details":"limit reached ∙ resets 3pm (America/New_York)"}`), now, utcF)
	want := time.Date(2026, 10, 20, 15, 0, 0, 0, ny)
	if !f.ResetAt.Equal(want) || f.Estimated {
		t.Errorf("ResetAt = %v estimated=%v", f.ResetAt, f.Estimated)
	}
	f, _ = Classify([]byte(`{"error":"rate_limit"}`), now, utcF)
	if !f.ResetAt.Equal(now.Add(DefaultCooldown)) || !f.Estimated {
		t.Errorf("fallback ResetAt = %v estimated=%v", f.ResetAt, f.Estimated)
	}
	f, _ = Classify([]byte(`{"error":"overloaded"}`), now, utcF)
	if !f.ResetAt.IsZero() {
		t.Error("transient failures carry no reset time")
	}
}

func TestParseReset(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Time
		ok   bool
	}{
		{"resets 3pm (America/New_York)", time.Date(2026, 10, 20, 15, 0, 0, 0, ny), true},
		{"resets 11am (America/New_York)", time.Date(2026, 10, 21, 11, 0, 0, 0, ny), true}, // already past today
		{"resets at 3:30pm (America/New_York)", time.Date(2026, 10, 20, 15, 30, 0, 0, ny), true},
		{"resets 12am (America/New_York)", time.Date(2026, 10, 21, 0, 0, 0, 0, ny), true},
		{"resets 12pm (America/New_York)", time.Date(2026, 10, 21, 12, 0, 0, 0, ny), true}, // noon == now, not after
		{"resets Oct 24, 2pm (America/New_York)", time.Date(2026, 10, 24, 14, 0, 0, 0, ny), true},
		{"resets October 24th at 2pm (America/New_York)", time.Date(2026, 10, 24, 14, 0, 0, 0, ny), true},
		{"resets in 2 hours", now.Add(2 * time.Hour), true},
		{"resets in 45 minutes", now.Add(45 * time.Minute), true},
		{"resets in 1 day", now.Add(24 * time.Hour), true},
		{"resets 3pm (Mars/Olympus)", time.Time{}, false},
		{"resets Nov 30, 2pm (America/New_York)", time.Time{}, false}, // > 8 days
		{"resets 13pm", time.Time{}, false},
		{"nothing here", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := ParseReset(c.msg, now, utcF)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("%q: got %v, %v; want %v, %v", c.msg, got, ok, c.want, c.ok)
		}
	}
}

func TestParseResetUsesLocalZoneWhenUnnamed(t *testing.T) {
	got, ok := ParseReset("resets 3pm", now, ny)
	if !ok || !got.Equal(time.Date(2026, 10, 20, 15, 0, 0, 0, ny)) {
		t.Errorf("got %v, %v", got, ok)
	}
}
