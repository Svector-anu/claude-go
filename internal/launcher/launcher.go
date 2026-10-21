// Package launcher runs the real Claude Code executable as a supervised child
// and performs account handoffs by stopping it and resuming the same session
// under another account's profile.
//
// Invariants:
//   - at most one child process is alive at any time;
//   - a handoff is reported successful only after the replacement has started;
//   - a failed handoff restarts the original account rather than leaving the user with nothing.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Svector-anu/claude-go/internal/claudeargs"
	"github.com/Svector-anu/claude-go/internal/control"
	"github.com/Svector-anu/claude-go/internal/history"
	"github.com/Svector-anu/claude-go/internal/modinstall"
)

const (
	EnvConfigDir = "CLAUDE_CONFIG_DIR"
	EnvWrapped   = "CLAUDE_GO_WRAPPED"
	// EnvSocket tells Claude-side extensions where the controller listens.
	EnvSocket = "CLAUDE_GO_SOCKET"

	defaultGrace = 5 * time.Second
	defaultPoll  = 200 * time.Millisecond
)

// Spec describes one child process.
type Spec struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// Process is a started child.
type Process interface {
	PID() int
	// Wait blocks until exit and returns the shell-style exit status.
	Wait() (int, error)
	Signal(os.Signal) error
}

// Starter starts child processes. Tests substitute a fake.
type Starter interface {
	Start(Spec) (Process, error)
}

// Account is a launchable identity: its ID and the directory holding its Claude
// Code state. Default accounts run with CLAUDE_CONFIG_DIR unset, because Claude
// derives a different Keychain item name from an explicit value.
type Account struct {
	ID        string
	Label     string
	ConfigDir string
	Default   bool
}

// Name is how the account is shown to the user.
func (a Account) Name() string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

// AutoDecision is the answer to a failure report.
type AutoDecision struct {
	// Target is the account to continue on; nil keeps the current one running.
	Target *Account
	// Line is the single status line printed when switching.
	Line string
	// Reason is a short, human-readable cause recorded in the handoff history.
	Reason string
	// Note is printed once when the session ends, e.g. when no account is left.
	Note string
	// Continue, when non-empty, is sent to the resumed session so Claude picks
	// up the request the limit cut short. Only set when the interruption is
	// confirmed, never for manual handoffs.
	Continue string
}

// HandoffRequest asks the session to continue on another account.
type HandoffRequest struct {
	ID string
	To string
	// Accepted, if set, is called once: with nil when the request passed
	// validation and the running child is about to be stopped, or with the
	// reason it was refused (the child is then untouched).
	Accepted func(error)
	// Done, if set, is called once with the final outcome.
	Done func(ok bool, message string)
}

// Outcome describes one handoff attempt for the history.
type Outcome struct {
	From, To Account
	Trigger  string
	Reason   string
	Result   string
	Message  string
}

// Phase is where the session is in its life.
type Phase string

const (
	PhaseStarting   Phase = "starting"
	PhaseRunning    Phase = "running"
	PhaseHandingOff Phase = "handing_off"
	PhaseEnded      Phase = "ended"
)

// Snapshot is a consistent view of the session for status displays.
type Snapshot struct {
	Phase        Phase
	Account      Account
	SessionID    string
	HandoffBlock string
	// Attention is set when the user may need to act (no account left, a switch
	// that cannot happen automatically).
	Attention string
	Handoffs  int
	StartedAt time.Time
}

// Session supervises one Claude Code conversation.
type Session struct {
	Starter    Starter
	ClaudePath string
	Plan       claudeargs.Plan
	Cwd        string
	Env        []string // base environment; CLAUDE_CONFIG_DIR is overridden per account
	Account    Account
	PID        int    // this wrapper's pid, used to key control files
	Socket     string // controller socket exported to Claude; empty when none is served
	SocketErr  string // why there is no socket, for status displays
	PluginDir  string // the claude-go mod, loaded into the child via CLAUDE_CODE_PLUGIN_DIRS; empty when not installed

	// Prepare resolves a handoff target and verifies it is usable (registered,
	// profile ready, logged in). Returning an error rejects the handoff and
	// leaves the running child untouched.
	Prepare func(ctx context.Context, accountID string) (Account, error)
	// HasTranscript reports whether the session has a non-empty transcript to resume.
	HasTranscript func(configDir, sessionID string) bool

	// Auto decides what to do about a failure report from a hook. Nil disables
	// automatic switching.
	Auto func(ctx context.Context, ev control.Event, cur Account) AutoDecision
	// Detect is polled while the child runs and may report a failure the hook
	// did not (for example a usage limit recorded in the transcript). since is
	// when the current child started: anything older belongs to an earlier
	// process or account and must be ignored.
	Detect func(ctx context.Context, cur Account, since time.Time) *control.Event
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// OnSwitch is told which account is running after every successful handoff.
	OnSwitch func(Account)
	// OnOutcome is told how every handoff attempt ended, including refusals.
	OnOutcome func(Outcome)

	Control control.Dir // empty disables handoff and locking
	// Requests delivers handoff requests from in-process front ends (the socket).
	Requests <-chan HandoffRequest
	Signals  <-chan os.Signal
	Log      io.Writer

	Grace     time.Duration // SIGTERM -> SIGKILL escalation
	PollEvery time.Duration

	exitNote string

	mu         sync.Mutex
	snap       Snapshot
	childStart time.Time
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ChildStartedAt is when the current child process was started.
func (s *Session) ChildStartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childStart
}

type waitResult struct {
	code int
	err  error
}

type handoff struct {
	req     *HandoffRequest // nil for automatic switches
	target  Account
	line    string // status line for automatic switches
	reason  string
	trigger string
	// carryOn is sent to the resumed session (automatic, confirmed interruptions only).
	carryOn string
}

// Snapshot returns the current state of the session.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *Session) update(fn func(*Snapshot)) {
	s.mu.Lock()
	fn(&s.snap)
	s.mu.Unlock()
}

// Run starts the child and supervises it until it exits for good. It returns
// the child's exit status.
func (s *Session) Run(ctx context.Context) (int, error) {
	s.update(func(sn *Snapshot) {
		*sn = Snapshot{Phase: PhaseStarting, Account: s.Account, SessionID: s.Plan.SessionID,
			HandoffBlock: s.Plan.HandoffBlock, StartedAt: time.Now().UTC()}
	})
	defer s.update(func(sn *Snapshot) { sn.Phase = PhaseEnded })

	var info control.Info
	if s.Control != "" {
		if s.Plan.SessionID != "" {
			lock, err := s.Control.LockSession(s.Plan.SessionID)
			if err != nil {
				return 0, err
			}
			defer func() { _ = lock.Release() }()
		}
		info = control.Info{
			PID: s.PID, SessionID: s.Plan.SessionID, Account: s.Account.ID,
			Cwd: s.Cwd, StartedAt: time.Now().UTC(), HandoffBlock: s.Plan.HandoffBlock,
			Socket: s.Socket, SocketError: s.SocketErr,
		}
		if err := s.Control.Register(info); err != nil {
			return 0, err
		}
		defer s.Control.Unregister(s.PID)
	}

	cur := s.Account
	proc, err := s.start(cur, s.Plan.Args)
	if err != nil {
		return 0, err
	}
	s.update(func(sn *Snapshot) { sn.Phase = PhaseRunning })
	for {
		code, ho, err := s.supervise(ctx, proc, cur)
		if err != nil {
			return 0, err
		}
		if ho == nil {
			if s.exitNote != "" {
				fmt.Fprintln(s.Log, s.exitNote)
			}
			return code, nil
		}
		next, replacement, err := s.switchTo(cur, ho)
		if err != nil {
			return 0, err
		}
		cur, proc = next, replacement
		s.update(func(sn *Snapshot) {
			sn.Phase, sn.Account, sn.Attention = PhaseRunning, cur, s.exitNote
			sn.Handoffs++
		})
		if s.OnSwitch != nil {
			s.OnSwitch(cur)
		}
		if s.Control != "" {
			info.Account = cur.ID
			if err := s.Control.Update(info); err != nil {
				s.logf("could not update registration: %v", err)
			}
		}
	}
}

func (s *Session) start(acct Account, args []string) (Process, error) {
	p, err := s.Starter.Start(Spec{Path: s.ClaudePath, Args: args, Dir: s.Cwd, Env: s.childEnv(acct)})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", s.ClaudePath, err)
	}
	s.mu.Lock()
	s.childStart = s.now()
	s.mu.Unlock()
	return p, nil
}

func (s *Session) childEnv(acct Account) []string {
	env := append(ProfileEnv(s.Env, acct), EnvWrapped+"="+strconv.Itoa(s.PID))
	if s.Socket != "" {
		env = append(env, EnvSocket+"="+s.Socket)
	}
	if s.PluginDir != "" {
		env = withPluginDir(env, s.PluginDir)
	}
	return env
}

// withPluginDir adds dir to CLAUDE_CODE_PLUGIN_DIRS, keeping whatever the user already set.
func withPluginDir(env []string, dir string) []string {
	existing := ""
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, modinstall.EnvPluginDirs+"="); ok {
			existing = v
			continue
		}
		out = append(out, kv)
	}
	return append(out, modinstall.EnvPluginDirs+"="+modinstall.MergePluginDirs(existing, dir))
}

// ProfileEnv returns base with claude-go's own variables removed and
// CLAUDE_CONFIG_DIR set for acct (or left unset for the default account).
func ProfileEnv(base []string, acct Account) []string {
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, EnvConfigDir+"=") || strings.HasPrefix(kv, EnvWrapped+"=") || strings.HasPrefix(kv, EnvSocket+"=") {
			continue
		}
		env = append(env, kv)
	}
	if !acct.Default {
		env = append(env, EnvConfigDir+"="+acct.ConfigDir)
	}
	return env
}

// supervise returns when the child exits on its own (handoff == nil) or when a
// valid handoff was requested and the child has been stopped.
func (s *Session) supervise(ctx context.Context, proc Process, cur Account) (int, *handoff, error) {
	done := make(chan waitResult, 1)
	go func() {
		code, err := proc.Wait()
		done <- waitResult{code, err}
	}()

	var tick <-chan time.Time
	if s.Control != "" {
		t := time.NewTicker(orDefault(s.PollEvery, defaultPoll))
		defer t.Stop()
		tick = t.C
	}
	ctxDone := ctx.Done()

	for {
		select {
		case r := <-done:
			return r.code, nil, nil
		case sig := <-s.Signals:
			// Ctrl+C is delivered to the whole foreground process group by the
			// terminal; forwarding it too would interrupt the child twice.
			if sig != os.Interrupt {
				_ = proc.Signal(sig)
			}
		case <-ctxDone:
			_ = proc.Signal(syscall.SIGTERM)
			ctxDone = nil
		case hr := <-s.Requests:
			if ho, handled, err := s.handleRequest(ctx, proc, done, cur, hr); handled {
				return 0, ho, err
			}
		case <-tick:
			if ho, handled, err := s.checkFailure(ctx, proc, done, cur); handled {
				return 0, ho, err
			}
			req, err := s.Control.TakeRequest(s.PID)
			if err != nil {
				s.logf("%v", err)
				continue
			}
			if req == nil {
				continue
			}
			hr := HandoffRequest{ID: req.ID, To: req.To, Done: func(ok bool, msg string) {
				if err := s.Control.Reply(s.PID, control.Result{ID: req.ID, OK: ok, Message: msg}); err != nil {
					s.logf("could not publish handoff result: %v", err)
				}
			}}
			if ho, handled, err := s.handleRequest(ctx, proc, done, cur, hr); handled {
				return 0, ho, err
			}
		}
	}
}

// handleRequest validates a manual request. handled is true when the
// supervise loop must return: a handoff is ready (child stopped) or stopping failed.
func (s *Session) handleRequest(ctx context.Context, proc Process, done <-chan waitResult, cur Account, hr HandoffRequest) (*handoff, bool, error) {
	target, err := s.vet(ctx, hr, cur)
	if err != nil {
		s.refuse(hr, cur, err)
		return nil, false, nil
	}
	if hr.Accepted != nil {
		hr.Accepted(nil)
	}
	s.update(func(sn *Snapshot) { sn.Phase = PhaseHandingOff })
	if err := s.stop(proc, done); err != nil {
		if hr.Done != nil {
			hr.Done(false, err.Error())
		}
		return nil, true, err
	}
	return &handoff{req: &hr, target: target, trigger: history.TriggerManual, reason: "requested"}, true, nil
}

func (s *Session) refuse(hr HandoffRequest, cur Account, err error) {
	// Record first: whoever is told the answer can already find it in the history.
	s.outcome(Outcome{From: cur, To: Account{ID: hr.To}, Trigger: history.TriggerManual, Reason: "requested",
		Result: history.ResultRejected, Message: err.Error()})
	if hr.Accepted != nil {
		hr.Accepted(err)
	}
	if hr.Done != nil {
		hr.Done(false, err.Error())
	}
}

// checkFailure consumes a pending hook report. handled is true when the
// supervise loop must return: either a switch was prepared (ho != nil, child
// stopped) or stopping failed (err != nil).
func (s *Session) checkFailure(ctx context.Context, proc Process, done <-chan waitResult, cur Account) (ho *handoff, handled bool, err error) {
	ev, evErr := s.Control.TakeEvent(s.PID)
	if evErr != nil {
		s.logf("%v", evErr)
		return nil, false, nil
	}
	since := s.ChildStartedAt()
	if ev != nil && !ev.At.IsZero() && ev.At.Before(since) {
		ev = nil // reported by an earlier process, before the last switch: not about this account
	}
	if ev == nil && s.Detect != nil && s.Auto != nil {
		ev = s.Detect(ctx, cur, since)
	}
	if ev == nil || s.Auto == nil {
		return nil, false, nil
	}
	d := s.Auto(ctx, *ev, cur)
	if d.Note != "" {
		s.exitNote = d.Note
		s.update(func(sn *Snapshot) { sn.Attention = d.Note })
	}
	if d.Target == nil {
		if d.Note != "" {
			s.outcome(Outcome{From: cur, Trigger: history.TriggerAuto, Reason: d.Reason, Result: history.ResultUnavailable, Message: d.Note})
		}
		return nil, false, nil
	}
	if s.Plan.HandoffBlock != "" {
		s.exitNote = fmt.Sprintf("Account %s cannot continue, and this session cannot switch automatically (%s)", cur.Name(), s.Plan.HandoffBlock)
		s.update(func(sn *Snapshot) { sn.Attention = s.exitNote })
		s.outcome(Outcome{From: cur, To: *d.Target, Trigger: history.TriggerAuto, Reason: d.Reason, Result: history.ResultRejected, Message: s.exitNote})
		return nil, false, nil
	}
	s.update(func(sn *Snapshot) { sn.Phase = PhaseHandingOff })
	if err := s.stop(proc, done); err != nil {
		return nil, true, err
	}
	// Anything the stopped child reported while exiting belongs to the old account.
	_, _ = s.Control.TakeEvent(s.PID)
	s.exitNote = ""
	return &handoff{target: *d.Target, line: d.Line, reason: d.Reason, trigger: history.TriggerAuto, carryOn: d.Continue}, true, nil
}

func (s *Session) vet(ctx context.Context, req HandoffRequest, cur Account) (Account, error) {
	switch {
	case s.Plan.HandoffBlock != "":
		return Account{}, fmt.Errorf("handoff unavailable for this session: %s", s.Plan.HandoffBlock)
	case req.To == "":
		return Account{}, errors.New("handoff request names no target account")
	case req.To == cur.ID:
		return Account{}, fmt.Errorf("session is already running on account %q", cur.ID)
	case s.Prepare == nil:
		return Account{}, errors.New("handoff is not configured")
	}
	target, err := s.Prepare(ctx, req.To)
	if err != nil {
		return Account{}, fmt.Errorf("cannot hand off to %q: %w", req.To, err)
	}
	return target, nil
}

// stop asks the child to exit, escalating to SIGKILL after the grace period.
// Once Wait returns the process is gone, so no further transcript writes can occur.
func (s *Session) stop(proc Process, done <-chan waitResult) error {
	grace := orDefault(s.Grace, defaultGrace)
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = proc.Signal(syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
		return fmt.Errorf("claude (pid %d) did not exit after SIGKILL", proc.PID())
	}
}

// switchTo launches the replacement after the old child has exited. If the
// replacement cannot start, the original account is restarted.
func (s *Session) switchTo(cur Account, ho *handoff) (Account, Process, error) {
	args := s.Plan.Args      // what the replacement runs
	plainArgs := s.Plan.Args // what a recovery restart of the original account runs
	resumed := s.HasTranscript != nil && s.HasTranscript(cur.ConfigDir, s.Plan.SessionID)
	continued := false
	if resumed {
		plainArgs = claudeargs.ResumeArgs(s.Plan.Args, s.Plan.SessionID)
		args = plainArgs
		if ho.carryOn != "" {
			args = claudeargs.ResumeArgsWithPrompt(s.Plan.Args, s.Plan.SessionID, ho.carryOn)
			continued = true
		}
	}
	if ho.line != "" {
		fmt.Fprintln(s.Log, ho.line)
	} else {
		s.logf("handing session %s from %q to %q", s.Plan.SessionID, cur.ID, ho.target.ID)
	}
	out := Outcome{From: cur, To: ho.target, Trigger: ho.trigger, Reason: ho.reason}

	proc, err := s.start(ho.target, args)
	if err == nil {
		how := "resumed"
		if !resumed {
			how = "restarted (no transcript yet)"
		}
		msg := fmt.Sprintf("session %s %s on account %q (pid %d)", s.Plan.SessionID, how, ho.target.ID, proc.PID())
		if continued {
			msg += ", carrying on with the interrupted request"
		}
		s.reply(ho.req, true, msg)
		out.Result, out.Message = history.ResultOK, msg
		s.outcome(out)
		return ho.target, proc, nil
	}

	s.logf("replacement failed to start: %v; restarting %q", err, cur.ID)
	proc, recoverErr := s.start(cur, plainArgs)
	if recoverErr != nil {
		msg := fmt.Sprintf("handoff failed (%v) and the original account could not be restarted (%v)", err, recoverErr)
		s.reply(ho.req, false, msg)
		out.Result, out.Message = history.ResultFailed, msg
		s.outcome(out)
		return Account{}, nil, fmt.Errorf("handoff failed (%v) and recovery failed: %w", err, recoverErr)
	}
	msg := fmt.Sprintf("handoff failed (%v); session restarted on original account %q", err, cur.ID)
	s.reply(ho.req, false, msg)
	out.Result, out.Message = history.ResultRecovered, msg
	s.outcome(out)
	return cur, proc, nil
}

func (s *Session) outcome(o Outcome) {
	if s.OnOutcome != nil {
		s.OnOutcome(o)
	}
}

func (s *Session) reply(req *HandoffRequest, ok bool, msg string) {
	if req == nil {
		if !ok {
			s.logf("%s", msg)
		}
		return
	}
	if req.Done != nil {
		req.Done(ok, msg)
	}
}

func (s *Session) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "claude-go: "+format+"\n", args...)
	}
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}
