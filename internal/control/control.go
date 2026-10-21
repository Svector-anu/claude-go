// Package control is the file-based channel between a running claude-go
// wrapper and other claude-go invocations (for example `claude-go handoff`).
//
// Everything lives under <state>/runtime and is keyed by wrapper PID, so
// concurrent wrappers never see each other's requests. Files are written
// atomically with mode 0600.
package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-go/internal/fsx"
	"claude-go/internal/limits"
)

const (
	dirMode      os.FileMode = 0o700
	fileMode     os.FileMode = 0o600
	pollInterval             = 50 * time.Millisecond
)

// ErrBusy means a handoff request for that wrapper is already pending.
var ErrBusy = errors.New("another handoff request is already pending")

// Info describes one running wrapper.
type Info struct {
	PID          int       `json:"pid"`
	SessionID    string    `json:"session_id,omitempty"`
	Account      string    `json:"account"`
	Cwd          string    `json:"cwd"`
	StartedAt    time.Time `json:"started_at"`
	HandoffBlock string    `json:"handoff_block,omitempty"`
	// Socket is the controller's Unix socket, when one is served.
	Socket string `json:"socket,omitempty"`
	// SocketError says why no socket is served (the core works without one).
	SocketError string `json:"socket_error,omitempty"`
}

// Request asks a wrapper to hand its session to another account.
type Request struct {
	ID string `json:"id"`
	To string `json:"to"`
}

// Event is a failure report for a wrapped session, from a Claude Code hook or
// from the transcript watcher.
type Event struct {
	SessionID string         `json:"session_id,omitempty"`
	Failure   limits.Failure `json:"failure"`
	At        time.Time      `json:"at"`
	// Source says where the report came from: "hook" or "transcript".
	Source string `json:"source,omitempty"`
}

// Result is the wrapper's answer to a Request.
type Result struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Dir is the runtime directory.
type Dir string

// processAlive is a variable so tests can fake liveness.
var processAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (d Dir) file(pid int, suffix string) string {
	return filepath.Join(string(d), strconv.Itoa(pid)+suffix)
}

func (d Dir) ensure() error {
	if err := os.MkdirAll(string(d), dirMode); err != nil {
		return fmt.Errorf("create runtime dir: %w", err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return fsx.WriteFileAtomic(path, data, fileMode)
}

// Register publishes info for this wrapper, discarding any request or result a
// previous process with the same PID left behind.
func (d Dir) Register(info Info) error {
	if err := d.ensure(); err != nil {
		return err
	}
	for _, s := range []string{".handoff", ".result"} {
		if err := os.Remove(d.file(info.PID, s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear stale %s: %w", s, err)
		}
	}
	return writeJSON(d.file(info.PID, ".json"), info)
}

// Update rewrites this wrapper's registration without touching request or
// result files, so a just-published handoff answer is not lost.
func (d Dir) Update(info Info) error {
	return writeJSON(d.file(info.PID, ".json"), info)
}

// Unregister removes pid's registration and any pending request. A published
// result is left for the requester to consume, because the wrapper may exit
// immediately after reporting a failed handoff; Register clears leftovers.
func (d Dir) Unregister(pid int) {
	for _, s := range []string{".json", ".handoff", ".event"} {
		_ = os.Remove(d.file(pid, s))
	}
}

// Active lists wrappers whose process is still alive, ordered by PID.
// Registrations whose process has died are ignored.
func (d Dir) Active() ([]Info, error) {
	entries, err := os.ReadDir(string(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime dir: %w", err)
	}
	var out []Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(string(d), e.Name()))
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(data, &info) != nil || info.PID == 0 || !processAlive(info.PID) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// LockSession takes the exclusive per-session lock so two wrappers can never
// drive the same session. It fails immediately if another holder exists.
func (d Dir) LockSession(sessionID string) (*fsx.Lock, error) {
	if err := d.ensure(); err != nil {
		return nil, err
	}
	l, err := fsx.AcquireLock(filepath.Join(string(d), "session-"+sessionID+".lock"), 0)
	if errors.Is(err, fsx.ErrLockTimeout) {
		return nil, fmt.Errorf("session %s is already running in another claude-go process", sessionID)
	}
	return l, err
}

// TakeRequest consumes the pending request for pid, or returns nil if none.
// An unreadable request is removed and reported as an error so it cannot wedge the wrapper.
func (d Dir) TakeRequest(pid int) (*Request, error) {
	path := d.file(pid, ".handoff")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read handoff request: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("consume handoff request: %w", err)
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil || req.ID == "" {
		return nil, errors.New("discarded malformed handoff request")
	}
	return &req, nil
}

// Reply publishes the outcome of req.
func (d Dir) Reply(pid int, res Result) error {
	return writeJSON(d.file(pid, ".result"), res)
}

// Submit asks the wrapper at pid to hand off to account `to` and waits for its answer.
func (d Dir) Submit(ctx context.Context, pid int, to string, timeout time.Duration) (Result, error) {
	if !processAlive(pid) {
		return Result{}, fmt.Errorf("no running claude-go process with pid %d", pid)
	}
	if err := d.ensure(); err != nil {
		return Result{}, err
	}
	reqPath := d.file(pid, ".handoff")
	if _, err := os.Stat(reqPath); err == nil {
		return Result{}, ErrBusy
	}
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return Result{}, fmt.Errorf("generate request id: %w", err)
	}
	req := Request{ID: hex.EncodeToString(idBytes[:]), To: to}
	resPath := d.file(pid, ".result")
	_ = os.Remove(resPath)
	if err := writeJSON(reqPath, req); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(reqPath)
			return Result{}, fmt.Errorf("no answer from pid %d within %s (request withdrawn)", pid, timeout)
		case <-ticker.C:
			data, err := os.ReadFile(resPath)
			if err != nil {
				if !processAlive(pid) {
					return Result{}, fmt.Errorf("process %d exited before answering", pid)
				}
				continue
			}
			var res Result
			if json.Unmarshal(data, &res) != nil || res.ID != req.ID {
				continue
			}
			_ = os.Remove(resPath)
			return res, nil
		}
	}
}

// PostEvent publishes a failure report for the wrapper at pid. A newer report
// replaces an unread older one.
func (d Dir) PostEvent(pid int, ev Event) error {
	if err := d.ensure(); err != nil {
		return err
	}
	return writeJSON(d.file(pid, ".event"), ev)
}

// TakeEvent consumes the pending failure report for pid, or returns nil if none.
// An unreadable report is removed and reported as an error.
func (d Dir) TakeEvent(pid int) (*Event, error) {
	path := d.file(pid, ".event")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read failure event: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("consume failure event: %w", err)
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, errors.New("discarded malformed failure event")
	}
	return &ev, nil
}
