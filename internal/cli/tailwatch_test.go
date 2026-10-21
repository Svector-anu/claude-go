package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-go/internal/availability"
	"claude-go/internal/history"
	"claude-go/internal/ipc"
)

// harnessNow is the frozen clock every CLI test runs on.
var harnessNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func tsAt(d time.Duration) string { return harnessNow.Add(d).Format("2006-01-02T15:04:05.000Z") }

func rejectedLine(at time.Duration, uuid, window string, resets time.Time, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"timestamp":%q,"isApiErrorMessage":true,"apiErrorStatus":429,"error":"rate_limit","quotaLimits":{"status":"rejected","resetsAt":%d,"rateLimitType":%q},"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`,
		uuid, tsAt(at), resets.Unix(), window, text)
}

func promptLine(at time.Duration) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u1","timestamp":%q,"message":{"role":"user","content":"keep going"}}`, tsAt(at))
}

type liveRig struct {
	h    *harness
	cl   *ipc.Client
	end  func()
	sid  string
	path string
}

func startRig(t *testing.T, accounts ...string) *liveRig {
	t.Helper()
	h := newShortHarness(t)
	logins := map[string]string{}
	for _, a := range accounts {
		logins[a] = a + "@example.com"
	}
	h.env.Runner = identityRunner(h, logins)
	for _, a := range accounts {
		h.run("accounts", "add", a, "--login")
	}
	cl, end := liveSession(t, h, "--account", accounts[0])
	sid := sessionLaunches(h)[0].Args[1]
	return &liveRig{h: h, cl: cl, end: end, sid: sid,
		path: filepath.Join(h.userHome, ".claude", "projects", "-work", sid+".jsonl")}
}

func (r *liveRig) write(t *testing.T, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func (r *liveRig) state(t *testing.T) availability.File {
	t.Helper()
	st, err := availability.Load(filepath.Join(r.h.home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTranscriptLimitSwitchesWithNoHookAtAll(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	resets := harnessNow.Add(95 * time.Minute)
	r.write(t, promptLine(0), rejectedLine(time.Second, "L1", "five_hour", resets, "You've hit your session limit · resets 1:35pm (UTC)"))

	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	if got := cfgDir(sessionLaunches(r.h)[1]); !strings.HasSuffix(got, "/profiles/b") {
		t.Errorf("continued on %q", got)
	}
	if got := sessionLaunches(r.h)[1].Args[:2]; got[0] != "--resume" || got[1] != r.sid {
		t.Errorf("resume args = %v", got)
	}
	ea := r.state(t).Accounts["a"]
	if ea.Status != availability.Exhausted || ea.Estimated || !ea.Until.Equal(resets.UTC()) {
		t.Errorf("account a = %+v, want exhausted until the exact reset %v", ea, resets)
	}
	waitUntil(t, func() bool {
		hist, _ := history.Tail(filepath.Join(r.h.home, "handoffs.jsonl"), 5)
		return len(hist) == 1 && hist[0].Result == "ok" && hist[0].Reason == "usage limit (5-hour)"
	})
	// The old limit line is still the last line of the transcript. It must not fire again for the new account.
	time.Sleep(700 * time.Millisecond)
	if n := len(sessionLaunches(r.h)); n != 2 {
		t.Errorf("launches = %d: the same limit was handled twice", n)
	}
	if st := r.state(t); st.Accounts["b"].Status != "" {
		t.Errorf("the new account was marked unavailable by the old account's limit: %+v", st.Accounts["b"])
	}
}

func TestALimitFromBeforeTheSessionStartedIsIgnored(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	r.write(t, rejectedLine(-time.Hour, "old", "seven_day", harnessNow.Add(24*time.Hour), "You've hit your weekly limit · resets tomorrow"))
	time.Sleep(900 * time.Millisecond)
	if n := len(sessionLaunches(r.h)); n != 1 {
		t.Errorf("launches = %d: yesterday's limit moved the session", n)
	}
	if len(r.state(t).Accounts) != 0 {
		t.Errorf("state changed: %+v", r.state(t).Accounts)
	}
}

func TestALimitTheUserHasAlreadyMovedPastIsIgnored(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	r.write(t, rejectedLine(time.Second, "L1", "five_hour", harnessNow.Add(time.Hour), "limit"), promptLine(time.Minute))
	time.Sleep(900 * time.Millisecond)
	if n := len(sessionLaunches(r.h)); n != 1 {
		t.Errorf("launches = %d", n)
	}
}

func TestWeeklyAndModelLimitsAreNamedAndClassified(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	r.write(t, rejectedLine(time.Second, "L1", "seven_day", harnessNow.Add(48*time.Hour), "You've hit your weekly limit · resets Oct 22 at 3am (UTC)"))
	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	waitUntil(t, func() bool {
		hist, _ := history.Tail(filepath.Join(r.h.home, "handoffs.jsonl"), 5)
		return len(hist) == 1 && strings.Contains(hist[0].Reason, "weekly")
	})
}

func TestModelSpecificTranscriptLimitKeepsTheAccountAsALastResort(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	r.write(t, rejectedLine(time.Second, "L1", "seven_day_opus", harnessNow.Add(5*time.Hour), "You've hit your Opus limit · resets 5pm"))
	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	if got := r.state(t).Accounts["a"].Status; got != availability.ModelLimited {
		t.Errorf("account a status = %q, want model_limited", got)
	}
}

func TestNoAccountLeftIsReportedOnceNotOnEveryPoll(t *testing.T) {
	r := startRig(t, "a")
	defer r.end()
	r.write(t, rejectedLine(time.Second, "L1", "five_hour", harnessNow.Add(time.Hour), "You've hit your session limit"))
	waitUntil(t, func() bool {
		s, err := r.cl.Status(context.Background())
		return err == nil && s.Session.Attention != ""
	})
	time.Sleep(900 * time.Millisecond) // several poll intervals
	hist, _ := history.Tail(filepath.Join(r.h.home, "handoffs.jsonl"), 10)
	if len(hist) != 1 || hist[0].Result != "unavailable" {
		t.Errorf("history = %+v; the limit must be handled exactly once", hist)
	}
	if n := len(sessionLaunches(r.h)); n != 1 {
		t.Errorf("launches = %d", n)
	}
}

func TestTheHookGetsTheExactResetTimeFromTheTranscript(t *testing.T) {
	r := startRig(t, "a", "b")
	defer r.end()
	resets := harnessNow.Add(70 * time.Minute)
	r.write(t, rejectedLine(time.Second, "L1", "five_hour", resets, "limit"))
	// The hook reports a bare rate_limit (no reset in its text); the transcript supplies the exact time.
	r.h.hook(t, itoa(os.Getpid()), `{"error":"rate_limit"}`)
	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	ea := r.state(t).Accounts["a"]
	if ea.Estimated || !ea.Until.Equal(resets.UTC()) {
		t.Errorf("account a = %+v, want the transcript's exact reset %v", ea, resets)
	}
}

func TestAStaleHookReportFromBeforeTheSwitchIsDropped(t *testing.T) {
	r := startRig(t, "a", "b", "c")
	defer r.end()
	r.write(t, rejectedLine(time.Second, "L1", "five_hour", harnessNow.Add(time.Hour), "limit"))
	waitUntil(t, func() bool { return len(sessionLaunches(r.h)) == 2 })
	// The old process's hook arrives late, reporting a time before the new child started.
	// (The harness clock is frozen, so "before" means an earlier timestamp.)
	if err := os.WriteFile(filepath.Join(r.h.home, "runtime", itoa(os.Getpid())+".event"),
		[]byte(fmt.Sprintf(`{"failure":{"kind":"account_limit","error":"rate_limit"},"at":%q,"source":"hook"}`, tsAt(-time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if n := len(sessionLaunches(r.h)); n != 2 {
		t.Errorf("launches = %d: a late report from the old account moved the session again", n)
	}
	if got := r.state(t).Accounts["b"].Status; got != "" {
		t.Errorf("the new account was blamed for the old account's limit: %q", got)
	}
}
