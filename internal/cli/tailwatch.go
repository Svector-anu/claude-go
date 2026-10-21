package cli

import (
	"context"
	"os"
	"time"

	"github.com/Svector-anu/claude-go/internal/control"
	"github.com/Svector-anu/claude-go/internal/launcher"
	"github.com/Svector-anu/claude-go/internal/limits"
	"github.com/Svector-anu/claude-go/internal/transcript"
)

const lookupEvery = time.Second

// tailWatcher notices a usage limit by reading the end of the session's own
// transcript. Claude records a structured `quotaLimits` entry there (window,
// exact reset time), so this needs no hook and no message parsing.
type tailWatcher struct {
	app       *app
	sessionID string

	dir       string
	path      string
	lookedAt  time.Time
	size      int64
	modTime   time.Time
	handledID string
}

// detect returns a failure event when the transcript ends at a limit written
// since the current child started, and nil otherwise. A limit is reported once.
func (w *tailWatcher) detect(_ context.Context, cur launcher.Account, since time.Time) *control.Event {
	if w.sessionID == "" {
		return nil
	}
	if w.path == "" || w.dir != cur.ConfigDir {
		if time.Since(w.lookedAt) < lookupEvery {
			return nil
		}
		w.lookedAt = time.Now()
		p, ok := transcript.Find(cur.ConfigDir, w.sessionID)
		if !ok {
			return nil
		}
		w.dir, w.path, w.size, w.modTime = cur.ConfigDir, p, 0, time.Time{}
	}
	fi, err := os.Stat(w.path)
	if err != nil || (fi.Size() == w.size && fi.ModTime().Equal(w.modTime)) {
		return nil
	}
	w.size, w.modTime = fi.Size(), fi.ModTime()

	l, ok, err := transcript.CurrentLimit(w.path, since)
	if err != nil || !ok || l.UUID == w.handledID {
		return nil
	}
	w.handledID = l.UUID
	return &control.Event{SessionID: w.sessionID, Source: "transcript", At: l.At, Failure: failureFrom(l)}
}

func failureFrom(l transcript.Limit) limits.Failure {
	kind := limits.AccountLimit
	if l.IsModelSpecific() {
		kind = limits.ModelLimit
	}
	return limits.Failure{Kind: kind, Error: "rate_limit", Message: l.Message, ResetAt: l.ResetsAt}
}

// windowName makes Claude's window name readable in the history.
func windowName(t string) string {
	switch t {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "weekly"
	default:
		return t
	}
}

// transcriptFlushWait is how long a hook-reported limit waits for Claude to
// write its transcript record. A variable so tests can shorten it.
var transcriptFlushWait = time.Second

// awaitRecordedLimit returns the limit recorded in the transcript. When
// immediate is false it keeps looking for up to transcriptFlushWait.
func (a *app) awaitRecordedLimit(cur launcher.Account, sessionID string, since time.Time, immediate bool) *transcript.Limit {
	deadline := time.Now().Add(transcriptFlushWait)
	for {
		if l, ok := a.recordedLimit(cur, sessionID, since); ok {
			return &l
		}
		if immediate || time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// recordedLimit reads the limit Claude recorded for this session, if the
// transcript ends at one written since the child started.
func (a *app) recordedLimit(cur launcher.Account, sessionID string, since time.Time) (transcript.Limit, bool) {
	p, ok := transcript.Find(cur.ConfigDir, sessionID)
	if !ok {
		return transcript.Limit{}, false
	}
	l, ok, err := transcript.CurrentLimit(p, since)
	return l, ok && err == nil
}
