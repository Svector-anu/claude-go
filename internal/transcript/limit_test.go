package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func ts(offset time.Duration) string { return base.Add(offset).Format("2006-01-02T15:04:05.000Z") }

// The shapes below follow records observed in real transcripts (field names and
// values as Claude writes them), with identifiers and times replaced.
func limitLine(at time.Duration, uuid, kind string, resets int64) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"timestamp":%q,"isApiErrorMessage":true,"apiErrorStatus":429,"error":"rate_limit","quotaLimits":{"status":"rejected","resetsAt":%d,"unifiedRateLimitFallbackAvailable":false,"rateLimitType":%q,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false},"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 8:10am (America/New_York)"}]}}`, uuid, ts(at), resets, kind)
}

func userLine(at time.Duration, meta bool) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u","timestamp":%q,"isMeta":%t,"message":{"role":"user","content":"do the thing"}}`, ts(at), meta)
}

func assistantLine(at time.Duration) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"a","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`, ts(at))
}

const (
	systemLine = `{"type":"system","subtype":"informational","timestamp":"2026-10-20T12:00:05.000Z"}`
	queueLine  = `{"type":"queue-operation","operation":"enqueue"}`
)

func write(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCurrentLimitFindsTheRejectedQuotaAtTheEnd(t *testing.T) {
	resets := base.Add(2 * time.Hour).Unix()
	p := write(t, userLine(0, false), limitLine(time.Second, "L1", "five_hour", resets), systemLine, queueLine)
	l, ok, err := CurrentLimit(p, base)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if l.Type != "five_hour" || l.ResetsAt.Unix() != resets || l.UUID != "L1" || !l.At.Equal(base.Add(time.Second)) {
		t.Errorf("limit = %+v", l)
	}
	if !strings.Contains(l.Message, "session limit") {
		t.Errorf("message = %q", l.Message)
	}
	if l.IsModelSpecific() {
		t.Error("a five-hour window is not model specific")
	}
}

func TestMetaLinesAfterTheLimitDoNotMeanTheConversationMovedOn(t *testing.T) {
	p := write(t, userLine(0, false), limitLine(time.Second, "L1", "seven_day", base.Unix()+3600), userLine(2*time.Second, true), systemLine)
	if _, ok, _ := CurrentLimit(p, base); !ok {
		t.Error("meta/system lines after the limit must not hide it")
	}
}

func TestALimitTheConversationHasMovedPastIsHistory(t *testing.T) {
	for name, after := range map[string]string{
		"user typed again": userLine(time.Minute, false),
		"claude answered":  assistantLine(time.Minute),
	} {
		p := write(t, limitLine(time.Second, "L1", "five_hour", base.Unix()+3600), after)
		if _, ok, _ := CurrentLimit(p, base); ok {
			t.Errorf("%s: a past limit was reported as current", name)
		}
	}
}

func TestALimitOlderThanSinceBelongsToAnEarlierProcess(t *testing.T) {
	p := write(t, limitLine(-time.Hour, "old", "five_hour", base.Unix()+3600))
	if _, ok, _ := CurrentLimit(p, base); ok {
		t.Fatal("a limit written before this process started must be ignored")
	}
	if _, ok, _ := CurrentLimit(p, base.Add(-2*time.Hour)); !ok {
		t.Error("it is current when the process started earlier")
	}
	// Exactly at since counts.
	p = write(t, limitLine(0, "now", "five_hour", base.Unix()+3600))
	if _, ok, _ := CurrentLimit(p, base); !ok {
		t.Error("a limit written exactly at since is current")
	}
}

func TestNonLimitErrorsAndOtherQuotaStatesAreIgnored(t *testing.T) {
	overloaded := fmt.Sprintf(`{"type":"assistant","uuid":"x","timestamp":%q,"isApiErrorMessage":true,"apiErrorStatus":529,"error":"overloaded","message":{"content":[{"type":"text","text":"Overloaded"}]}}`, ts(time.Second))
	allowed := strings.Replace(limitLine(time.Second, "w", "five_hour", base.Unix()+60), `"status":"rejected"`, `"status":"allowed_warning"`, 1)
	noQuota := fmt.Sprintf(`{"type":"assistant","uuid":"y","timestamp":%q,"isApiErrorMessage":true,"apiErrorStatus":429,"error":"rate_limit","message":{"content":[{"type":"text","text":"slow down"}]}}`, ts(time.Second))
	for name, line := range map[string]string{"overloaded": overloaded, "warning only": allowed, "429 without a quota record": noQuota} {
		if _, ok, _ := CurrentLimit(write(t, userLine(0, false), line), base); ok {
			t.Errorf("%s was treated as an account limit", name)
		}
	}
}

func TestCurrentLimitOnMissingEmptyAndTornFiles(t *testing.T) {
	if _, ok, err := CurrentLimit(filepath.Join(t.TempDir(), "none.jsonl"), base); ok || err != nil {
		t.Errorf("missing file: ok=%v err=%v", ok, err)
	}
	empty := filepath.Join(t.TempDir(), "e.jsonl")
	os.WriteFile(empty, nil, 0o600)
	if _, ok, err := CurrentLimit(empty, base); ok || err != nil {
		t.Errorf("empty file: ok=%v err=%v", ok, err)
	}
	// A line cut off mid-write is skipped; the limit before it still counts.
	p := write(t, limitLine(time.Second, "L1", "five_hour", base.Unix()+3600), `{"type":"user","uuid":"u","timest`)
	if _, ok, _ := CurrentLimit(p, base); !ok {
		t.Error("a torn final line must not hide the limit before it")
	}
	if _, ok, _ := CurrentLimit(write(t, "not json", "{}", "[]"), base); ok {
		t.Error("garbage produced a limit")
	}
}

func TestOnlyTheTailOfAHugeTranscriptIsRead(t *testing.T) {
	filler := userLine(0, false)
	var lines []string
	for i := 0; i < (3<<20)/len(filler); i++ {
		lines = append(lines, filler)
	}
	// A limit far from the end is not looked for...
	p := write(t, append([]string{limitLine(time.Second, "buried", "five_hour", base.Unix()+3600)}, lines...)...)
	if _, ok, _ := CurrentLimit(p, base); ok {
		t.Error("found a limit that is not at the end")
	}
	// ...but one at the end is found, and the partial first line of the window is not misparsed.
	p = write(t, append(lines, limitLine(time.Minute, "end", "five_hour", base.Unix()+3600))...)
	l, ok, err := CurrentLimit(p, base)
	if err != nil || !ok || l.UUID != "end" {
		t.Errorf("l=%+v ok=%v err=%v", l, ok, err)
	}
}

func TestModelSpecificLimitTypes(t *testing.T) {
	for typ, want := range map[string]bool{"five_hour": false, "seven_day": false, "seven_day_opus": true, "SEVEN_DAY_SONNET": true, "": false} {
		if got := (Limit{Type: typ}).IsModelSpecific(); got != want {
			t.Errorf("%q: %v", typ, got)
		}
	}
}

func TestContentCanBeAStringOrBlocks(t *testing.T) {
	line := fmt.Sprintf(`{"type":"assistant","uuid":"s","timestamp":%q,"isApiErrorMessage":true,"error":"rate_limit","quotaLimits":{"status":"rejected","rateLimitType":"seven_day","resetsAt":%d},"message":{"content":"You've hit your weekly limit"}}`, ts(time.Second), base.Unix()+3600)
	l, ok, _ := CurrentLimit(write(t, line), base)
	if !ok || l.Message != "You've hit your weekly limit" {
		t.Errorf("l=%+v ok=%v", l, ok)
	}
}

// TestRealTranscripts checks the parser against every limit record in the
// transcripts on this machine. It reads local conversation files, so it only
// runs when asked: CLAUDE_GO_REAL_TRANSCRIPTS=1 go test ./internal/transcript -run Real -v
func TestRealTranscripts(t *testing.T) {
	if os.Getenv("CLAUDE_GO_REAL_TRANSCRIPTS") == "" {
		t.Skip("set CLAUDE_GO_REAL_TRANSCRIPTS=1 to run against ~/.claude/projects")
	}
	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	counts := map[string]int{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			if !strings.Contains(sc.Text(), `"quotaLimits"`) {
				continue
			}
			var line tailLine
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				continue
			}
			l, ok := line.limit()
			if !ok {
				continue
			}
			if l.Type != "five_hour" && l.Type != "seven_day" {
				t.Errorf("unexpected window type %q", l.Type)
			}
			if l.ResetsAt.IsZero() || l.At.IsZero() || l.UUID == "" || l.Message == "" {
				t.Errorf("incomplete record: %+v", l)
			}
			counts[l.Type]++
		}
		fh.Close()
	}
	t.Logf("parsed real limit records: %v", counts)
	if len(counts) == 0 {
		t.Log("no limit records on this machine")
	}
}
