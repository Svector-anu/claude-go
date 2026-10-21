package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Svector-anu/claude-go/internal/availability"
	"github.com/Svector-anu/claude-go/internal/buildinfo"
	"github.com/Svector-anu/claude-go/internal/history"
	"github.com/Svector-anu/claude-go/internal/ipc"
	"github.com/Svector-anu/claude-go/internal/launcher"
	"github.com/Svector-anu/claude-go/internal/registry"
)

// controller answers the extension-facing API from the running session. It
// only reports and requests: every account decision stays in launcher.Session
// and the availability package.
type controller struct {
	app       *app
	session   *launcher.Session
	requests  chan launcher.HandoffRequest
	startedAt time.Time
}

func newController(a *app) *controller {
	return &controller{app: a, requests: make(chan launcher.HandoffRequest), startedAt: a.env.Now().UTC()}
}

func accountRef(a launcher.Account) ipc.AccountRef { return ipc.AccountRef{ID: a.ID, Label: a.Name()} }

func toEntry(e history.Entry) ipc.HistoryEntry {
	return ipc.HistoryEntry{At: e.At, SessionID: e.SessionID, From: e.From, To: e.To, Trigger: e.Trigger,
		Reason: e.Reason, Result: e.Result, Message: e.Message}
}

// Status implements ipc.Controller.
func (c *controller) Status() ipc.Status {
	sn := c.session.Snapshot()
	view := ipc.SessionView{
		ID: sn.SessionID, Cwd: c.session.Cwd, Account: accountRef(sn.Account), Phase: string(sn.Phase),
		HandoffAvailable: sn.HandoffBlock == "", HandoffBlock: sn.HandoffBlock,
		Attention: sn.Attention, Handoffs: sn.Handoffs,
	}
	if sn.Attention != "" {
		view.Action = c.recoveryAction()
	}
	st := ipc.Status{APIVersion: ipc.APIVersion, PID: c.app.env.Getpid(), Version: buildinfo.Version,
		StartedAt: c.startedAt, Session: view}
	if last, err := history.Tail(c.app.layout.History, 1); err == nil && len(last) == 1 {
		e := toEntry(last[0])
		st.LastHandoff = &e
	}
	return st
}

// recoveryAction names the one command that most likely unblocks the user.
func (c *controller) recoveryAction() string {
	reg, err := registry.Load(c.app.layout.Accounts)
	if err != nil {
		return ""
	}
	st, err := availability.Load(c.app.layout.State)
	if err != nil {
		return ""
	}
	now := c.app.env.Now()
	for _, a := range reg.Accounts {
		if e, blocked := st.Blocked(a.ID, now); blocked && e.Status == availability.AuthFailed {
			return "claude-go accounts login " + a.ID
		}
	}
	return "claude-go accounts add NAME --login"
}

// Accounts implements ipc.Controller.
func (c *controller) Accounts() ([]ipc.AccountView, error) {
	reg, err := registry.Load(c.app.layout.Accounts)
	if err != nil {
		return nil, err
	}
	st, err := availability.Load(c.app.layout.State)
	if err != nil {
		return nil, err
	}
	now := c.app.env.Now()
	current := c.session.Snapshot().Account.ID
	out := make([]ipc.AccountView, 0, len(reg.Accounts))
	for _, a := range reg.Accounts {
		v := ipc.AccountView{ID: a.ID, Label: nameOf(a), Default: a.Default, Current: a.ID == current, State: ipc.StateReady}
		if e, blocked := st.Blocked(a.ID, now); blocked {
			switch e.Status {
			case availability.Exhausted:
				v.State = ipc.StateLimit
			case availability.ModelLimited:
				v.State = ipc.StateModelLimit
			case availability.AuthFailed:
				v.State = ipc.StateNeedsLogin
			default:
				v.State = ipc.StateBlocked
			}
			if !e.Until.IsZero() {
				until := e.Until
				v.Until, v.Estimated = &until, e.Estimated
			}
		}
		if u, ok := st.Usage[a.ID]; ok {
			seen := &ipc.UsageSeen{SeenAt: u.SeenAt}
			for _, w := range u.Windows {
				win := ipc.Window{Kind: w.Kind, PercentUsed: w.PercentUsed}
				if !w.ResetsAt.IsZero() {
					r := w.ResetsAt
					win.ResetsAt = &r
				}
				seen.Windows = append(seen.Windows, win)
			}
			v.LastUsage = seen
		}
		out = append(out, v)
	}
	return out, nil
}

// History implements ipc.Controller.
func (c *controller) History(limit int) ([]ipc.HistoryEntry, error) {
	entries, err := history.Tail(c.app.layout.History, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ipc.HistoryEntry, len(entries))
	for i, e := range entries {
		out[i] = toEntry(e)
	}
	return out, nil
}

// RequestHandoff implements ipc.Controller. It returns once the session has
// accepted or refused the request; the handoff itself continues afterwards.
func (c *controller) RequestHandoff(ctx context.Context, to string) (<-chan struct{}, error) {
	finished := make(chan struct{})
	accepted := make(chan error, 1)
	req := launcher.HandoffRequest{
		ID:       to,
		To:       to,
		Accepted: func(err error) { accepted <- err },
		Done:     func(bool, string) { close(finished) },
	}
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case err := <-accepted:
		if err != nil {
			return nil, err
		}
		return finished, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RecordUsage implements ipc.Controller: the windows belong to the account the session runs on.
func (c *controller) RecordUsage(windows []ipc.Window) error {
	cur := c.session.Snapshot().Account.ID
	if cur == "" {
		return errors.New("no active account")
	}
	seen := availability.UsageSeen{SeenAt: c.app.env.Now().UTC()}
	for _, w := range windows {
		win := availability.Window{Kind: w.Kind, PercentUsed: w.PercentUsed}
		if w.ResetsAt != nil {
			win.ResetsAt = w.ResetsAt.UTC()
		}
		seen.Windows = append(seen.Windows, win)
	}
	// The status line reports on every refresh; skip a write that changes nothing.
	if st, err := availability.Load(c.app.layout.State); err == nil {
		if prev, ok := st.Usage[cur]; ok && sameWindows(prev.Windows, seen.Windows) && seen.SeenAt.Sub(prev.SeenAt) < usageRefresh {
			return nil
		}
	}
	return availability.RecordUsage(c.app.layout.State, cur, seen)
}

const usageRefresh = time.Minute

func sameWindows(a, b []availability.Window) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].PercentUsed != b[i].PercentUsed || !a[i].ResetsAt.Equal(b[i].ResetsAt) {
			return false
		}
	}
	return true
}

// record persists a handoff outcome to the history.
func (c *controller) record(o launcher.Outcome) {
	e := history.Entry{
		At: c.app.env.Now().UTC(), SessionID: c.session.Plan.SessionID, From: o.From.ID, To: o.To.ID,
		Trigger: o.Trigger, Reason: o.Reason, Result: o.Result, Message: o.Message,
	}
	if err := history.Append(c.app.layout.History, e); err != nil {
		fmt.Fprintf(c.app.env.Stderr, "claude-go: could not record handoff history: %v\n", err)
	}
}

// startIPC serves the controller on a Unix socket next to the runtime files.
// Failure is not fatal: the core works without the socket, and the reason is
// kept for `claude-go status` instead of being printed into the session.
func (c *controller) startIPC() (stop func(), socket, problem string) {
	socket = filepath.Join(c.app.layout.Runtime, strconv.Itoa(c.app.env.Getpid())+".sock")
	srv := ipc.NewServer(c, c.app.env.Now)
	if err := srv.Listen(socket); err != nil {
		return func() {}, "", err.Error()
	}
	return func() { _ = srv.Close() }, socket, ""
}
