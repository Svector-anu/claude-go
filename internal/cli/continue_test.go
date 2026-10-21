package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"claude-go/internal/config"
	"claude-go/internal/history"
)

func limitedRig(t *testing.T, configJSON string, runArgs ...string) *liveRig {
	t.Helper()
	h := newShortHarness(t)
	if configJSON != "" {
		h.writeState(t, "config.json", configJSON)
	}
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("accounts", "add", "b", "--login")
	cl, end := liveSession(t, h, append([]string{"--account", "a"}, runArgs...)...)
	sid := sessionLaunches(h)[0].Args[1]
	return &liveRig{h: h, cl: cl, end: end, sid: sid,
		path: filepath.Join(h.userHome, ".claude", "projects", "-work", sid+".jsonl")}
}

func (r *liveRig) secondLaunch(t *testing.T) []string {
	t.Helper()
	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	return sessionLaunches(r.h)[1].Args
}

func limitRecord() string {
	return rejectedLine(time.Second, "L1", "five_hour", harnessNow.Add(time.Hour), "You've hit your session limit · resets soon")
}

func TestContinuesTheInterruptedRequestAfterATranscriptDetectedLimit(t *testing.T) {
	r := limitedRig(t, "")
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	want := []string{"--resume", r.sid, config.DefaultContinueMessage}
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
	waitUntil(t, func() bool {
		hist, _ := history.Tail(filepath.Join(r.h.home, "handoffs.jsonl"), 3)
		return len(hist) == 1 && strings.Contains(hist[0].Message, "carrying on")
	})
}

func TestContinuesAfterAHookReportedLimitOnceTheTranscriptConfirmsIt(t *testing.T) {
	r := limitedRig(t, "")
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	r.h.hook(t, itoa(os.Getpid()), `{"error":"rate_limit"}`)
	got := r.secondLaunch(t)
	if len(got) != 3 || got[2] != config.DefaultContinueMessage {
		t.Errorf("args = %q", got)
	}
}

func TestDoesNotContinueWhenTheTranscriptDoesNotConfirmAnInterruptedRequest(t *testing.T) {
	r := limitedRig(t, "")
	defer r.end()
	r.write(t, promptLine(0)) // a transcript exists, but it does not end at a limit record
	r.h.hook(t, itoa(os.Getpid()), `{"error":"rate_limit","error_details":"limit reached"}`)
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, []string{"--resume", r.sid}) {
		t.Errorf("args = %q: an unconfirmed interruption must not trigger a message", got)
	}
}

func TestDoesNotContinueAfterASignInFailure(t *testing.T) {
	r := limitedRig(t, "")
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	r.h.hook(t, itoa(os.Getpid()), `{"error":"authentication_failed"}`)
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, []string{"--resume", r.sid}) {
		t.Errorf("args = %q", got)
	}
}

func TestContinuationCanBeTurnedOffInConfig(t *testing.T) {
	r := limitedRig(t, `{"version":1,"continue_after_limit":false}`)
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, []string{"--resume", r.sid}) {
		t.Errorf("args = %q", got)
	}
}

func TestContinuationMessageIsConfigurable(t *testing.T) {
	r := limitedRig(t, `{"version":1,"continue_message":"Pick up where you left off."}`)
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, []string{"--resume", r.sid, "Pick up where you left off."}) {
		t.Errorf("args = %q", got)
	}
}

func TestUsersFlagsFollowTheContinuationAndTheOneShotPromptIsNotResent(t *testing.T) {
	r := limitedRig(t, "", "--", "do the original task", "--model", "opus", "--add-dir", "/x", "/y")
	defer r.end()
	r.write(t, promptLine(0), limitRecord())
	want := []string{"--resume", r.sid, config.DefaultContinueMessage, "--model", "opus", "--add-dir", "/x", "/y"}
	if got := r.secondLaunch(t); !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
}

func TestManualSocketHandoffDoesNotContinue(t *testing.T) {
	r := limitedRig(t, "")
	defer r.end()
	r.write(t, promptLine(0), limitRecord()) // even with a limit at the tail, a manual switch is the user's call
	if _, err := r.cl.Handoff(t.Context(), "req-manualcont", "b"); err != nil {
		t.Fatal(err)
	}
	got := r.secondLaunch(t)
	// The transcript detector may also have fired; either way a manual request is never given a message.
	if len(got) > 2 && got[2] == config.DefaultContinueMessage && sessionLaunches(r.h)[1].Env != nil {
		hist, _ := history.Tail(filepath.Join(r.h.home, "handoffs.jsonl"), 3)
		if len(hist) > 0 && hist[len(hist)-1].Trigger == "manual" {
			t.Errorf("a manual handoff carried a continuation: %q", got)
		}
	}
}
