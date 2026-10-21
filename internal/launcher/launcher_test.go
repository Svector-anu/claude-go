package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"claude-go/internal/claudeargs"
	"claude-go/internal/control"
	"claude-go/internal/limits"
)

const sid = "11111111-2222-4333-8444-555555555555"

type fakeProc struct {
	pid      int
	exit     chan int
	stubborn bool // ignores SIGTERM
	signals  []os.Signal
	mu       sync.Mutex
	starter  *fakeStarter
	once     sync.Once
}

func (p *fakeProc) PID() int { return p.pid }

func (p *fakeProc) Wait() (int, error) {
	code := <-p.exit
	p.once.Do(func() { atomic.AddInt32(&p.starter.live, -1) })
	return code, nil
}

func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	switch sig {
	case syscall.SIGKILL:
		p.finish(137)
	case syscall.SIGTERM:
		if !p.stubborn {
			p.finish(143)
		}
	}
	return nil
}

func (p *fakeProc) finish(code int) {
	select {
	case p.exit <- code:
	default:
	}
}

func (p *fakeProc) got() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

type fakeStarter struct {
	mu      sync.Mutex
	specs   []Spec
	procs   []*fakeProc
	live    int32
	maxLive int32
	// failAt lists 0-based start indices that fail.
	failAt   map[int]bool
	stubborn bool
}

func (f *fakeStarter) Start(spec Spec) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.specs)
	f.specs = append(f.specs, spec)
	if f.failAt[idx] {
		return nil, errors.New("exec failed")
	}
	p := &fakeProc{pid: 1000 + idx, exit: make(chan int, 1), stubborn: f.stubborn, starter: f}
	f.procs = append(f.procs, p)
	n := atomic.AddInt32(&f.live, 1)
	if n > f.maxLive {
		f.maxLive = n
	}
	return p, nil
}

func (f *fakeStarter) spec(i int) Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specs[i]
}

func (f *fakeStarter) proc(i int) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[i]
}

func (f *fakeStarter) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type rig struct {
	st      *fakeStarter
	sess    *Session
	ctl     control.Dir
	signals chan os.Signal
	log     *bytes.Buffer
	result  chan runResult
	prepare func(context.Context, string) (Account, error)
	hasTx   atomic.Bool
}

type runResult struct {
	code int
	err  error
}

func newRig(t *testing.T, args ...string) *rig {
	t.Helper()
	plan, err := claudeargs.Build(args, func() (string, error) { return sid, nil })
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		st:      &fakeStarter{},
		ctl:     control.Dir(filepath.Join(t.TempDir(), "runtime")),
		signals: make(chan os.Signal, 4),
		log:     &bytes.Buffer{},
		result:  make(chan runResult, 1),
	}
	r.prepare = func(_ context.Context, id string) (Account, error) {
		return Account{ID: id, ConfigDir: "/profiles/" + id}, nil
	}
	r.hasTx.Store(true)
	r.sess = &Session{
		Starter:       r.st,
		ClaudePath:    "/bin/claude",
		Plan:          plan,
		Cwd:           "/work/dir",
		Env:           []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/should/be/replaced", "CLAUDE_GO_WRAPPED=1"},
		Account:       Account{ID: "alpha", ConfigDir: "/profiles/alpha"},
		PID:           os.Getpid(),
		Prepare:       func(ctx context.Context, id string) (Account, error) { return r.prepare(ctx, id) },
		HasTranscript: func(string, string) bool { return r.hasTx.Load() },
		Control:       r.ctl,
		Signals:       r.signals,
		Log:           r.log,
		Grace:         80 * time.Millisecond,
		PollEvery:     5 * time.Millisecond,
	}
	return r
}

func (r *rig) start(t *testing.T) {
	t.Helper()
	go func() {
		code, err := r.sess.Run(context.Background())
		r.result <- runResult{code, err}
	}()
	waitFor(t, "first child", func() bool { return r.st.startCount() >= 1 })
}

func (r *rig) wait(t *testing.T) runResult {
	t.Helper()
	select {
	case res := <-r.result:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
		return runResult{}
	}
}

func (r *rig) handoff(t *testing.T, to string) control.Result {
	t.Helper()
	res, err := r.ctl.Submit(context.Background(), os.Getpid(), to, 3*time.Second)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return res
}

func envValue(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			val, n = strings.TrimPrefix(kv, key+"="), n+1
		}
	}
	return val, n
}

func TestLaunchPreservesArgsDirAndSetsProfileEnv(t *testing.T) {
	r := newRig(t, "--model", "opus", "fix it")
	r.start(t)
	spec := r.st.spec(0)
	want := []string{"--session-id", sid, "--model", "opus", "fix it"}
	if spec.Path != "/bin/claude" || spec.Dir != "/work/dir" || !reflect.DeepEqual(spec.Args, want) {
		t.Errorf("spec = %+v", spec)
	}
	if v, n := envValue(spec.Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/alpha" || n != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR = %q (x%d)", v, n)
	}
	if v, n := envValue(spec.Env, "CLAUDE_GO_WRAPPED"); v != fmt.Sprint(os.Getpid()) || n != 1 {
		t.Errorf("CLAUDE_GO_WRAPPED = %q (x%d)", v, n)
	}
	if v, _ := envValue(spec.Env, "PATH"); v != "/bin" {
		t.Errorf("PATH not preserved")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestExitStatusIsPropagated(t *testing.T) {
	for _, code := range []int{0, 1, 3, 130} {
		r := newRig(t)
		r.start(t)
		r.st.proc(0).finish(code)
		if res := r.wait(t); res.err != nil || res.code != code {
			t.Errorf("code %d: got %+v", code, res)
		}
	}
}

func TestStartFailureIsReportedAndCleansUp(t *testing.T) {
	r := newRig(t)
	r.st.failAt = map[int]bool{0: true}
	_, err := r.sess.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if active, _ := r.ctl.Active(); len(active) != 0 {
		t.Errorf("registration leaked: %+v", active)
	}
}

func TestSignalsAreForwardedExceptInterrupt(t *testing.T) {
	r := newRig(t)
	r.start(t)
	r.signals <- os.Interrupt
	r.signals <- syscall.SIGHUP
	waitFor(t, "SIGHUP forwarded", func() bool { return len(r.st.proc(0).got()) == 1 })
	if got := r.st.proc(0).got(); got[0] != syscall.SIGHUP {
		t.Errorf("signals = %v", got)
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestContextCancelTerminatesChild(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		code, err := r.sess.Run(ctx)
		r.result <- runResult{code, err}
	}()
	waitFor(t, "child", func() bool { return r.st.startCount() == 1 })
	cancel()
	if res := r.wait(t); res.code != 143 {
		t.Errorf("res = %+v", res)
	}
}

func TestRegistrationVisibleWhileRunningAndRemovedAfter(t *testing.T) {
	r := newRig(t)
	r.start(t)
	active, _ := r.ctl.Active()
	if len(active) != 1 || active[0].SessionID != sid || active[0].Account != "alpha" {
		t.Errorf("active = %+v", active)
	}
	r.st.proc(0).finish(0)
	r.wait(t)
	if active, _ := r.ctl.Active(); len(active) != 0 {
		t.Errorf("still registered: %+v", active)
	}
}

func TestHandoffResumesSameSessionOnTargetAccount(t *testing.T) {
	r := newRig(t, "--model", "opus", "initial prompt")
	r.start(t)

	res := r.handoff(t, "beta")
	if !res.OK {
		t.Fatalf("handoff failed: %s", res.Message)
	}
	if r.st.startCount() != 2 {
		t.Fatalf("starts = %d", r.st.startCount())
	}
	if got := r.st.proc(0).got(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Errorf("first child signals = %v", got)
	}
	second := r.st.spec(1)
	want := []string{"--resume", sid, "--model", "opus"}
	if !reflect.DeepEqual(second.Args, want) {
		t.Errorf("resume args = %v, want %v", second.Args, want)
	}
	if v, _ := envValue(second.Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/beta" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q", v)
	}
	if second.Dir != "/work/dir" {
		t.Errorf("working directory lost: %q", second.Dir)
	}
	if r.st.maxLive != 1 {
		t.Errorf("max concurrent children = %d", r.st.maxLive)
	}
	waitFor(t, "registration to name the new account", func() bool {
		active, _ := r.ctl.Active()
		return len(active) == 1 && active[0].Account == "beta"
	})

	r.st.proc(1).finish(7)
	if out := r.wait(t); out.code != 7 || out.err != nil {
		t.Errorf("final = %+v", out)
	}
}

func TestHandoffWithoutTranscriptRelaunchesFreshWithSameID(t *testing.T) {
	r := newRig(t)
	r.hasTx.Store(false)
	r.start(t)
	if res := r.handoff(t, "beta"); !res.OK {
		t.Fatal(res.Message)
	}
	want := []string{"--session-id", sid}
	if got := r.st.spec(1).Args; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestSecondHandoffReturnsToFirstAccount(t *testing.T) {
	r := newRig(t)
	r.start(t)
	if res := r.handoff(t, "beta"); !res.OK {
		t.Fatal(res.Message)
	}
	if res := r.handoff(t, "alpha"); !res.OK {
		t.Fatal(res.Message)
	}
	if v, _ := envValue(r.st.spec(2).Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/alpha" {
		t.Errorf("third launch dir = %q", v)
	}
	if r.st.maxLive != 1 {
		t.Errorf("max concurrent children = %d", r.st.maxLive)
	}
	r.st.proc(2).finish(0)
	r.wait(t)
}

func TestHandoffRejections(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		to      string
		prepare func(context.Context, string) (Account, error)
		wantMsg string
	}{
		{"same account", nil, "alpha", nil, "already running"},
		{"unknown session", []string{"--continue"}, "beta", nil, "handoff unavailable"},
		{"not logged in", nil, "beta", func(context.Context, string) (Account, error) { return Account{}, errors.New("not logged in") }, "not logged in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.args...)
			if c.prepare != nil {
				r.prepare = c.prepare
			}
			r.start(t)
			res := r.handoff(t, c.to)
			if res.OK || !strings.Contains(res.Message, c.wantMsg) {
				t.Errorf("res = %+v", res)
			}
			if got := r.st.proc(0).got(); len(got) != 0 {
				t.Errorf("running child was disturbed: %v", got)
			}
			if r.st.startCount() != 1 {
				t.Errorf("starts = %d", r.st.startCount())
			}
			r.st.proc(0).finish(0)
			r.wait(t)
		})
	}
}

func TestFailedHandoffRestartsOriginalAccount(t *testing.T) {
	r := newRig(t)
	r.st.failAt = map[int]bool{1: true}
	r.start(t)
	res := r.handoff(t, "beta")
	if res.OK || !strings.Contains(res.Message, "restarted on original account") {
		t.Fatalf("res = %+v", res)
	}
	if r.st.startCount() != 3 {
		t.Fatalf("starts = %d", r.st.startCount())
	}
	if v, _ := envValue(r.st.spec(2).Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/alpha" {
		t.Errorf("recovery launched on %q", v)
	}
	if got := r.st.spec(2).Args; !reflect.DeepEqual(got, []string{"--resume", sid}) {
		t.Errorf("recovery args = %v", got)
	}
	r.st.proc(1).finish(0)
	if out := r.wait(t); out.err != nil || out.code != 0 {
		t.Errorf("final = %+v", out)
	}
}

func TestFailedHandoffWithFailedRecoveryIsReported(t *testing.T) {
	r := newRig(t)
	r.st.failAt = map[int]bool{1: true, 2: true}
	r.start(t)
	res := r.handoff(t, "beta")
	if res.OK || !strings.Contains(res.Message, "could not be restarted") {
		t.Fatalf("res = %+v", res)
	}
	if out := r.wait(t); out.err == nil {
		t.Error("Run must return the failure")
	}
}

func TestStubbornChildIsKilledAfterGrace(t *testing.T) {
	r := newRig(t)
	r.st.stubborn = true
	r.start(t)
	if res := r.handoff(t, "beta"); !res.OK {
		t.Fatal(res.Message)
	}
	got := r.st.proc(0).got()
	if len(got) != 2 || got[0] != syscall.SIGTERM || got[1] != syscall.SIGKILL {
		t.Errorf("signals = %v", got)
	}
	if r.st.maxLive != 1 {
		t.Errorf("replacement started before old child exited (max live %d)", r.st.maxLive)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestSameSessionCannotRunTwice(t *testing.T) {
	r := newRig(t)
	r.start(t)
	dup := newRig(t)
	dup.sess.Control = r.ctl
	dup.sess.PID = os.Getpid() + 1
	_, err := dup.sess.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	if dup.st.startCount() != 0 {
		t.Error("duplicate session launched a child")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestExecStarterExitStatusEnvDirAndArgs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	s := ExecStarter{}
	p, err := s.Start(Spec{
		Path: "/bin/sh",
		Args: []string{"-c", `printf '%s|%s|%s %s' "$PWD" "$CG_TEST" "$1" "$2" > out; exit 7`, "sh", "a b", "c"},
		Dir:  dir,
		Env:  []string{"CG_TEST=ok", "PATH=/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait()
	if err != nil || code != 7 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got, _ := os.ReadFile(out)
	resolvedDir, _ := filepath.EvalSymlinks(dir)
	gotPWD, _, _ := strings.Cut(string(got), "|")
	if resolved, _ := filepath.EvalSymlinks(gotPWD); resolved != resolvedDir || !strings.HasSuffix(string(got), "|ok|a b c") {
		t.Errorf("child saw %q", got)
	}
}

func TestExecStarterReportsSignalDeathAsShellStatus(t *testing.T) {
	p, err := ExecStarter{}.Start(Spec{Path: "/bin/sleep", Args: []string{"30"}, Env: []string{"PATH=/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code, _ := p.Wait(); code != 128+int(syscall.SIGTERM) {
		t.Errorf("code = %d", code)
	}
}

func TestExecStarterMissingBinary(t *testing.T) {
	if _, err := (ExecStarter{}).Start(Spec{Path: "/definitely/not/here"}); err == nil {
		t.Fatal("expected error")
	}
}

func postLimit(t *testing.T, r *rig) {
	t.Helper()
	ev := control.Event{Failure: limits.Failure{Kind: limits.AccountLimit, Error: "rate_limit"}, At: time.Now()}
	if err := r.ctl.PostEvent(os.Getpid(), ev); err != nil {
		t.Fatal(err)
	}
}

func TestAutoSwitchOnFailureEvent(t *testing.T) {
	r := newRig(t, "--model", "opus", "prompt")
	var switched []string
	var mu sync.Mutex
	r.sess.OnSwitch = func(a Account) { mu.Lock(); switched = append(switched, a.ID); mu.Unlock() }
	r.sess.Auto = func(_ context.Context, ev control.Event, cur Account) AutoDecision {
		if ev.Failure.Kind != limits.AccountLimit || cur.ID != "alpha" {
			t.Errorf("unexpected input: %+v %+v", ev, cur)
		}
		return AutoDecision{
			Target: &Account{ID: "beta", Label: "Beta", ConfigDir: "/profiles/beta"},
			Line:   "Account Alpha hit its limit, continuing on Account Beta",
		}
	}
	r.start(t)
	postLimit(t, r)
	waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })

	if got := r.st.proc(0).got(); len(got) != 1 || got[0] != syscall.SIGTERM {
		t.Errorf("first child signals = %v", got)
	}
	if got, want := r.st.spec(1).Args, []string{"--resume", sid, "--model", "opus"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resume args = %v, want %v", got, want)
	}
	if v, _ := envValue(r.st.spec(1).Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/beta" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q", v)
	}
	if got := r.log.String(); got != "Account Alpha hit its limit, continuing on Account Beta\n" {
		t.Errorf("output must be exactly the one status line, got %q", got)
	}
	mu.Lock()
	if !reflect.DeepEqual(switched, []string{"beta"}) {
		t.Errorf("OnSwitch calls = %v", switched)
	}
	mu.Unlock()
	if r.st.maxLive != 1 {
		t.Errorf("max concurrent children = %d", r.st.maxLive)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestAutoSwitchToDefaultAccountUnsetsConfigDir(t *testing.T) {
	r := newRig(t)
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		return AutoDecision{Target: &Account{ID: "default", ConfigDir: "/home/u/.claude", Default: true}, Line: "switched"}
	}
	r.start(t)
	postLimit(t, r)
	waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })
	if v, n := envValue(r.st.spec(1).Env, "CLAUDE_CONFIG_DIR"); n != 0 {
		t.Errorf("default account must not export CLAUDE_CONFIG_DIR, got %q", v)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestDefaultAccountLaunchStripsInheritedConfigDir(t *testing.T) {
	r := newRig(t)
	r.sess.Account = Account{ID: "default", ConfigDir: "/home/u/.claude", Default: true}
	r.start(t)
	if _, n := envValue(r.st.spec(0).Env, "CLAUDE_CONFIG_DIR"); n != 0 {
		t.Error("inherited CLAUDE_CONFIG_DIR leaked into the default account")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestAutoNoTargetKeepsRunningAndNotesAtExit(t *testing.T) {
	r := newRig(t)
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		return AutoDecision{Note: "All accounts are at their limit; earliest reset 14:05"}
	}
	r.start(t)
	postLimit(t, r)
	time.Sleep(60 * time.Millisecond)
	if len(r.st.proc(0).got()) != 0 || r.st.startCount() != 1 {
		t.Fatal("child must keep running when no account is available")
	}
	if r.log.Len() != 0 {
		t.Errorf("nothing may be printed while the session is live: %q", r.log.String())
	}
	r.st.proc(0).finish(0)
	r.wait(t)
	if got := r.log.String(); got != "All accounts are at their limit; earliest reset 14:05\n" {
		t.Errorf("exit note = %q", got)
	}
}

func TestAutoSwitchRefusedWhenSessionCannotResume(t *testing.T) {
	r := newRig(t, "--continue")
	var called atomic.Bool
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		called.Store(true)
		return AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "switched"}
	}
	r.start(t)
	postLimit(t, r)
	waitFor(t, "auto to be consulted", called.Load)
	time.Sleep(40 * time.Millisecond)
	if len(r.st.proc(0).got()) != 0 || r.st.startCount() != 1 {
		t.Fatal("must not stop a session that cannot be resumed")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
	if !strings.Contains(r.log.String(), "cannot switch automatically") {
		t.Errorf("exit note = %q", r.log.String())
	}
}

func TestEventsAreIgnoredWithoutAutoHandler(t *testing.T) {
	r := newRig(t)
	r.start(t)
	postLimit(t, r)
	time.Sleep(40 * time.Millisecond)
	if ev, _ := r.ctl.TakeEvent(os.Getpid()); ev != nil {
		t.Error("event was not consumed")
	}
	if len(r.st.proc(0).got()) != 0 {
		t.Error("child disturbed")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestAutoSwitchFailureRestartsOriginal(t *testing.T) {
	r := newRig(t)
	r.st.failAt = map[int]bool{1: true}
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		return AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "Account A hit its limit, continuing on Account B"}
	}
	r.start(t)
	postLimit(t, r)
	waitFor(t, "recovery launch", func() bool { return r.st.startCount() == 3 })
	if v, _ := envValue(r.st.spec(2).Env, "CLAUDE_CONFIG_DIR"); v != "/profiles/alpha" {
		t.Errorf("recovery launched on %q", v)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func submit(t *testing.T, ch chan<- HandoffRequest, to string) (accepted chan error, done chan control.Result) {
	t.Helper()
	accepted, done = make(chan error, 1), make(chan control.Result, 1)
	ch <- HandoffRequest{ID: "r1", To: to,
		Accepted: func(err error) { accepted <- err },
		Done:     func(ok bool, msg string) { done <- control.Result{ID: "r1", OK: ok, Message: msg} },
	}
	return accepted, done
}

func outcomes(r *rig) (get func() []Outcome) {
	var mu sync.Mutex
	var got []Outcome
	r.sess.OnOutcome = func(o Outcome) { mu.Lock(); got = append(got, o); mu.Unlock() }
	return func() []Outcome { mu.Lock(); defer mu.Unlock(); return append([]Outcome(nil), got...) }
}

func TestInProcessRequestAcceptsBeforeStoppingThenReportsDone(t *testing.T) {
	r := newRig(t)
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	got := outcomes(r)
	r.start(t)

	accepted, done := submit(t, ch, "beta")
	if err := <-accepted; err != nil {
		t.Fatalf("accepted with %v", err)
	}
	res := <-done
	if !res.OK {
		t.Fatalf("done: %+v", res)
	}
	if r.st.startCount() != 2 || r.st.maxLive != 1 {
		t.Errorf("starts=%d maxLive=%d", r.st.startCount(), r.st.maxLive)
	}
	o := got()
	if len(o) != 1 || o[0].Result != "ok" || o[0].Trigger != "manual" || o[0].From.ID != "alpha" || o[0].To.ID != "beta" {
		t.Errorf("outcomes = %+v", o)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestInProcessRequestRejectionLeavesChildAloneAndIsRecorded(t *testing.T) {
	r := newRig(t)
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	got := outcomes(r)
	r.prepare = func(context.Context, string) (Account, error) { return Account{}, errors.New("not signed in") }
	r.start(t)

	accepted, done := submit(t, ch, "beta")
	err := <-accepted
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("accepted = %v", err)
	}
	if res := <-done; res.OK {
		t.Error("done must report failure")
	}
	if len(r.st.proc(0).got()) != 0 || r.st.startCount() != 1 {
		t.Error("a refused request disturbed the child")
	}
	if o := got(); len(o) != 1 || o[0].Result != "rejected" || !strings.Contains(o[0].Message, "not signed in") {
		t.Errorf("outcomes = %+v", o)
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestFailedAndRecoveredHandoffOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		failAt map[int]bool
		want   string
	}{
		{"recovered", map[int]bool{1: true}, "recovered"},
		{"failed", map[int]bool{1: true, 2: true}, "failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			ch := make(chan HandoffRequest, 1)
			r.sess.Requests = ch
			got := outcomes(r)
			r.st.failAt = c.failAt
			r.start(t)
			_, done := submit(t, ch, "beta")
			if res := <-done; res.OK {
				t.Fatal("expected failure")
			}
			o := got()
			if len(o) != 1 || o[0].Result != c.want {
				t.Fatalf("outcomes = %+v", o)
			}
			if c.want == "recovered" {
				r.st.proc(1).finish(0)
			}
			r.wait(t)
		})
	}
}

func TestAutoOutcomes(t *testing.T) {
	cases := []struct {
		name string
		d    AutoDecision
		args []string
		want string
	}{
		{"switched", AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "x", Reason: "usage limit"}, nil, "ok"},
		{"nothing available", AutoDecision{Note: "none left", Reason: "usage limit"}, nil, "unavailable"},
		{"cannot resume", AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "x", Reason: "usage limit"}, []string{"--continue"}, "rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, c.args...)
			got := outcomes(r)
			r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision { return c.d }
			r.start(t)
			postLimit(t, r)
			waitFor(t, "outcome", func() bool { return len(got()) == 1 })
			o := got()[0]
			if o.Result != c.want || o.Trigger != "auto" || o.Reason != "usage limit" {
				t.Errorf("outcome = %+v", o)
			}
			if c.want == "ok" {
				waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })
				r.st.proc(1).finish(0)
			} else {
				r.st.proc(0).finish(0)
			}
			r.wait(t)
		})
	}
}

func TestSnapshotTracksPhaseAccountAndAttention(t *testing.T) {
	r := newRig(t)
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		return AutoDecision{Note: "all accounts are at their limit"}
	}
	if got := r.sess.Snapshot().Phase; got != "" {
		t.Errorf("phase before Run = %q", got)
	}
	r.start(t)
	waitFor(t, "running", func() bool { return r.sess.Snapshot().Phase == PhaseRunning })
	sn := r.sess.Snapshot()
	if sn.Account.ID != "alpha" || sn.SessionID != sid || sn.StartedAt.IsZero() {
		t.Errorf("snapshot = %+v", sn)
	}

	postLimit(t, r)
	waitFor(t, "attention", func() bool { return r.sess.Snapshot().Attention != "" })

	_, done := submit(t, ch, "beta")
	<-done
	waitFor(t, "switched", func() bool { sn := r.sess.Snapshot(); return sn.Account.ID == "beta" && sn.Handoffs == 1 })
	if r.sess.Snapshot().Phase != PhaseRunning {
		t.Errorf("phase = %s", r.sess.Snapshot().Phase)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
	if r.sess.Snapshot().Phase != PhaseEnded {
		t.Errorf("final phase = %s", r.sess.Snapshot().Phase)
	}
}

func TestSocketIsExportedToChildAndReplacements(t *testing.T) {
	r := newRig(t)
	r.sess.Socket = "/run/cg.sock"
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	r.start(t)
	_, done := submit(t, ch, "beta")
	<-done
	for i := 0; i < 2; i++ {
		if v, n := envValue(r.st.spec(i).Env, "CLAUDE_GO_SOCKET"); v != "/run/cg.sock" || n != 1 {
			t.Errorf("launch %d: CLAUDE_GO_SOCKET=%q x%d", i, v, n)
		}
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestInheritedSocketVariableIsReplacedNotDuplicated(t *testing.T) {
	r := newRig(t)
	r.sess.Env = append(r.sess.Env, "CLAUDE_GO_SOCKET=/stale.sock")
	r.start(t)
	if v, n := envValue(r.st.spec(0).Env, "CLAUDE_GO_SOCKET"); n != 0 {
		t.Errorf("stale CLAUDE_GO_SOCKET leaked: %q", v)
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestDetectorTriggersASwitchWithoutAHook(t *testing.T) {
	r := newRig(t)
	got := outcomes(r)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r.sess.Now = func() time.Time { return clock }
	var fired atomic.Bool
	var sinceSeen atomic.Value
	r.sess.Detect = func(_ context.Context, cur Account, since time.Time) *control.Event {
		sinceSeen.Store(since)
		if fired.Swap(true) {
			return nil
		}
		return &control.Event{Source: "transcript", At: clock.Add(time.Second), Failure: limits.Failure{Kind: limits.AccountLimit}}
	}
	r.sess.Auto = func(_ context.Context, ev control.Event, cur Account) AutoDecision {
		if ev.Source != "transcript" {
			t.Errorf("event source = %q", ev.Source)
		}
		return AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "switched", Reason: "usage limit"}
	}
	r.start(t)
	waitFor(t, "switch", func() bool { return r.st.startCount() == 2 })
	waitFor(t, "outcome", func() bool { return len(got()) == 1 })
	if got()[0].Result != "ok" || got()[0].Trigger != "auto" {
		t.Errorf("outcome = %+v", got()[0])
	}
	if since, _ := sinceSeen.Load().(time.Time); !since.Equal(clock) {
		t.Errorf("detector was given since=%v, want the child start %v", since, clock)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestAReportOlderThanTheCurrentChildIsDropped(t *testing.T) {
	r := newRig(t)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r.sess.Now = func() time.Time { return clock }
	var called atomic.Bool
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision {
		called.Store(true)
		return AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "x"}
	}
	r.start(t)
	stale := control.Event{At: clock.Add(-time.Minute), Failure: limits.Failure{Kind: limits.AccountLimit}}
	if err := r.ctl.PostEvent(os.Getpid(), stale); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if called.Load() || r.st.startCount() != 1 {
		t.Error("a report from before this process started was acted on")
	}
	r.st.proc(0).finish(0)
	r.wait(t)
}

func TestTheDetectorSeesAFreshSinceAfterEverySwitch(t *testing.T) {
	r := newRig(t)
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC).Unix())
	r.sess.Now = func() time.Time { return time.Unix(now.Load(), 0).UTC() }
	var mu sync.Mutex
	var sinces []time.Time
	r.sess.Detect = func(_ context.Context, _ Account, since time.Time) *control.Event {
		mu.Lock()
		sinces = append(sinces, since)
		mu.Unlock()
		return nil
	}
	r.sess.Auto = func(context.Context, control.Event, Account) AutoDecision { return AutoDecision{} }
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	r.start(t)
	time.Sleep(30 * time.Millisecond)
	now.Add(600)
	_, done := submit(t, ch, "beta")
	<-done
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	first, last := sinces[0], sinces[len(sinces)-1]
	mu.Unlock()
	if !last.After(first) {
		t.Errorf("since did not advance with the new child: first=%v last=%v", first, last)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func autoWithContinue(msg string) func(context.Context, control.Event, Account) AutoDecision {
	return func(context.Context, control.Event, Account) AutoDecision {
		return AutoDecision{Target: &Account{ID: "beta", ConfigDir: "/p/beta"}, Line: "switched", Reason: "usage limit", Continue: msg}
	}
}

func TestAutoSwitchSendsTheContinuationRightAfterTheSessionId(t *testing.T) {
	r := newRig(t, "first prompt", "--model", "opus", "--add-dir", "a", "b")
	got := outcomes(r)
	r.sess.Auto = autoWithContinue("carry on")
	r.start(t)
	postLimit(t, r)
	waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })
	want := []string{"--resume", sid, "carry on", "--model", "opus", "--add-dir", "a", "b"}
	if args := r.st.spec(1).Args; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q\nwant   %q", args, want)
	}
	waitFor(t, "outcome", func() bool { return len(got()) == 1 })
	if !strings.Contains(got()[0].Message, "carrying on with the interrupted request") {
		t.Errorf("outcome = %q", got()[0].Message)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestNoContinuationWithoutATranscriptToResume(t *testing.T) {
	r := newRig(t)
	r.hasTx.Store(false)
	r.sess.Auto = autoWithContinue("carry on")
	r.start(t)
	postLimit(t, r)
	waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })
	if args := r.st.spec(1).Args; !reflect.DeepEqual(args, []string{"--session-id", sid}) {
		t.Errorf("args = %q: nothing to continue yet, so the fresh session must not be told to", args)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestNoContinuationWhenTheDecisionHasNone(t *testing.T) {
	r := newRig(t)
	r.sess.Auto = autoWithContinue("")
	r.start(t)
	postLimit(t, r)
	waitFor(t, "replacement", func() bool { return r.st.startCount() == 2 })
	if args := r.st.spec(1).Args; !reflect.DeepEqual(args, []string{"--resume", sid}) {
		t.Errorf("args = %q", args)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestManualHandoffNeverSendsAContinuation(t *testing.T) {
	r := newRig(t)
	r.sess.Auto = autoWithContinue("carry on") // must not influence a manual request
	ch := make(chan HandoffRequest, 1)
	r.sess.Requests = ch
	r.start(t)
	_, done := submit(t, ch, "beta")
	<-done
	if args := r.st.spec(1).Args; !reflect.DeepEqual(args, []string{"--resume", sid}) {
		t.Errorf("manual handoff args = %q", args)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}

func TestRecoveryRestartOfTheOriginalAccountDoesNotSendTheContinuation(t *testing.T) {
	r := newRig(t)
	r.st.failAt = map[int]bool{1: true}
	r.sess.Auto = autoWithContinue("carry on")
	r.start(t)
	postLimit(t, r)
	waitFor(t, "recovery launch", func() bool { return r.st.startCount() == 3 })
	if args := r.st.spec(1).Args; len(args) < 3 || args[2] != "carry on" {
		t.Errorf("the replacement should have been told to continue: %q", args)
	}
	if args := r.st.spec(2).Args; !reflect.DeepEqual(args, []string{"--resume", sid}) {
		t.Errorf("the original account (which just hit its limit) must be restarted plainly, got %q", args)
	}
	r.st.proc(1).finish(0)
	r.wait(t)
}
