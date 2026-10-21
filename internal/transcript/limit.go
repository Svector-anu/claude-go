package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// tailBytes bounds how much of a transcript is read to look for a limit. A
// transcript can be hundreds of megabytes; the interrupted turn is at the end.
const tailBytes = 256 << 10

// Limit is a usage limit Claude recorded in the transcript. The record is
// structured (`quotaLimits`), so nothing here depends on message wording.
type Limit struct {
	// Type is Claude's window name: "five_hour", "seven_day", or a model-specific one.
	Type string
	// ResetsAt is when the window resets, exactly as Claude reported it.
	ResetsAt time.Time
	// At is when Claude wrote the line.
	At time.Time
	// UUID identifies the transcript line, so one limit is never handled twice.
	UUID string
	// Message is the text Claude showed ("You've hit your session limit · resets …").
	Message string
}

// IsModelSpecific reports whether only one model family is limited.
func (l Limit) IsModelSpecific() bool {
	t := strings.ToLower(l.Type)
	for _, m := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

type tailLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	IsMeta    bool   `json:"isMeta"`
	APIError  bool   `json:"isApiErrorMessage"`
	Error     string `json:"error"`
	Quota     *struct {
		Status        string  `json:"status"`
		RateLimitType string  `json:"rateLimitType"`
		ResetsAt      float64 `json:"resetsAt"`
	} `json:"quotaLimits"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func (l tailLine) isConversation() bool {
	return (l.Type == "user" && !l.IsMeta) || l.Type == "assistant"
}

// limit returns the limit this line records, if it is one.
func (l tailLine) limit() (Limit, bool) {
	if l.Type != "assistant" || !l.APIError || l.Error != "rate_limit" || l.Quota == nil || l.Quota.Status != "rejected" {
		return Limit{}, false
	}
	out := Limit{Type: l.Quota.RateLimitType, UUID: l.UUID, Message: contentText(l.Message.Content)}
	if l.Quota.ResetsAt > 0 {
		out.ResetsAt = time.Unix(int64(l.Quota.ResetsAt), 0).UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		out.At = t.UTC()
	}
	return out, true
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// CurrentLimit reports whether the transcript at path ends at a usage limit:
// the last conversation line is a rejected-quota record written at or after
// since. A limit the conversation has moved past (the user typed again, or
// Claude answered) is history, and a limit older than since belongs to an
// earlier process or account, so neither is returned.
func CurrentLimit(path string, since time.Time) (Limit, bool, error) {
	data, err := readTail(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, err
	}
	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		raw := bytes.TrimSpace(lines[i])
		if len(raw) == 0 {
			continue
		}
		var line tailLine
		if json.Unmarshal(raw, &line) != nil || !line.isConversation() {
			continue // torn line, or not part of the conversation
		}
		l, ok := line.limit()
		if !ok || l.At.IsZero() || l.At.Before(since) {
			return Limit{}, false, nil
		}
		return l, true, nil
	}
	return Limit{}, false, nil
}

// readTail returns the last tailBytes of the file, starting at a line boundary.
func readTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	start := int64(0)
	if fi.Size() > tailBytes {
		start = fi.Size() - tailBytes
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if start > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			return nil, nil // one enormous line: nothing usable in the tail
		}
	}
	return buf, nil
}
