// Package history keeps a short, append-only record of handoff outcomes.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Svector-anu/claude-go/internal/fsx"
)

const (
	lockTimeout = 5 * time.Second
	// compactAbove is the file size that triggers trimming to keepLines.
	compactAbove = 256 << 10
	keepLines    = 200
	maxMessage   = 300
)

// Results of a handoff attempt.
const (
	ResultOK          = "ok"
	ResultFailed      = "failed"      // the replacement could not start and nothing was restored
	ResultRecovered   = "recovered"   // the replacement could not start; the original account was restarted
	ResultRejected    = "rejected"    // the request was refused before anything was stopped
	ResultUnavailable = "unavailable" // a switch was wanted but no account could take over
)

// Triggers of a handoff.
const (
	TriggerManual = "manual"
	TriggerAuto   = "auto"
)

// Entry is one recorded handoff outcome. Messages never contain credentials:
// they are built from account names and error text only.
type Entry struct {
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Trigger   string    `json:"trigger"`
	Reason    string    `json:"reason,omitempty"`
	Result    string    `json:"result"`
	Message   string    `json:"message,omitempty"`
}

func clip(s string) string {
	r := []rune(s)
	if len(r) > maxMessage {
		return string(r[:maxMessage]) + "…"
	}
	return s
}

// Append adds e to the log at path under an exclusive lock, trimming the file
// when it grows large.
func Append(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	lock, err := fsx.AcquireLock(path+".lock", lockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	e.Message = clip(e.Message)
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode history entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open history %s: %w", path, err)
	}
	// A crash can leave a last line without its newline; start on a fresh line
	// so the torn entry does not swallow this one.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append history: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close history: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > compactAbove {
		return compact(path)
	}
	return nil
}

func compact(path string) error {
	entries, err := readAll(path)
	if err != nil {
		return err
	}
	if len(entries) > keepLines {
		entries = entries[len(entries)-keepLines:]
	}
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode history entry: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return fsx.WriteFileAtomic(path, buf.Bytes(), 0o600)
}

// readAll returns every parseable entry. A line torn by a crash is skipped,
// because an append-only log must stay readable after an interrupted write.
func readAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open history %s: %w", path, err)
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Result != "" {
			out = append(out, e)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read history %s: %w", path, err)
	}
	return out, nil
}

// Tail returns the most recent n entries, oldest first.
func Tail(path string, n int) ([]Entry, error) {
	all, err := readAll(path)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
