package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/claudeargs"
	"github.com/Svector-anu/claude-go/internal/launcher"
)

type stubProc struct {
	pid  int
	exit chan int
}

func (p *stubProc) PID() int           { return p.pid }
func (p *stubProc) Wait() (int, error) { return <-p.exit, nil }
func (p *stubProc) Signal(s os.Signal) error {
	if s == syscall.SIGTERM || s == syscall.SIGKILL {
		select {
		case p.exit <- 143:
		default:
		}
	}
	return nil
}

// stubStarter records launches. Children exit with exitCode immediately unless block is set.
type stubStarter struct {
	mu       sync.Mutex
	specs    []launcher.Spec
	procs    []*stubProc
	exitCode int
	block    bool
}

func (s *stubStarter) Start(spec launcher.Spec) (launcher.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &stubProc{pid: 5000 + len(s.specs), exit: make(chan int, 1)}
	s.specs = append(s.specs, spec)
	s.procs = append(s.procs, p)
	if !s.block {
		p.exit <- s.exitCode
	}
	return p, nil
}

func (s *stubStarter) snapshot() []launcher.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]launcher.Spec(nil), s.specs...)
}

func (h *harness) starter() *stubStarter { return h.env.Starter.(*stubStarter) }

func (h *harness) addAccounts(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if code := h.run("accounts", "add", id); code != exitOK {
			t.Fatalf("add %s: %d %s", id, code, h.stderr.String())
		}
	}
}

func cfgDir(spec launcher.Spec) string {
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			return v
		}
	}
	return ""
}

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		in      []string
		account string
		rest    []string
		wantErr bool
	}{
		{nil, "", nil, false},
		{[]string{"--account", "a", "--model", "x"}, "a", []string{"--model", "x"}, false},
		{[]string{"--account=a", "--", "--version"}, "a", []string{"--version"}, false},
		{[]string{"--", "--account", "z"}, "", []string{"--account", "z"}, false},
		{[]string{"--model", "x"}, "", []string{"--model", "x"}, false},
		{[]string{"--account"}, "", nil, true},
	}
	for _, c := range cases {
		a, rest, err := splitRunArgs(c.in)
		if (err != nil) != c.wantErr || a != c.account || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%v: got %q %v %v", c.in, a, rest, err)
		}
	}
}

func TestRunLaunchesClaudeUnderAccountProfile(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "work")
	h.starter().exitCode = 4
	if code := h.run("run", "--model", "opus", "hello"); code != 4 {
		t.Fatalf("exit code = %d (stderr %s)", code, h.stderr.String())
	}
	specs := h.starter().snapshot()
	if len(specs) != 1 {
		t.Fatalf("launches = %d", len(specs))
	}
	sp := specs[0]
	if sp.Path != "/bin/claude" || sp.Dir != "/work" {
		t.Errorf("spec = %+v", sp)
	}
	if len(sp.Args) != 5 || sp.Args[0] != "--session-id" || !claudeargs.IsUUID(sp.Args[1]) ||
		!reflect.DeepEqual(sp.Args[2:], []string{"--model", "opus", "hello"}) {
		t.Errorf("args = %v", sp.Args)
	}
	wantDir := filepath.Join(h.home, "profiles", "work")
	if cfgDir(sp) != wantDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", cfgDir(sp), wantDir)
	}
	link, err := os.Readlink(filepath.Join(wantDir, "projects"))
	if err != nil || link != filepath.Join(h.userHome, ".claude", "projects") {
		t.Errorf("shared projects link = %q, %v", link, err)
	}
	if active, _ := os.ReadDir(filepath.Join(h.home, "runtime")); len(active) > 0 {
		for _, e := range active {
			if strings.HasSuffix(e.Name(), ".json") {
				t.Errorf("registration left behind: %s", e.Name())
			}
		}
	}
}

func TestRunPassthroughDoesNotInjectSessionOrLink(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "work")
	if code := h.run("run", "--", "mcp", "list"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	sp := h.starter().snapshot()[0]
	if !reflect.DeepEqual(sp.Args, []string{"mcp", "list"}) {
		t.Errorf("args = %v", sp.Args)
	}
}

func TestRunAccountSelection(t *testing.T) {
	h := newHarness(t)
	if code := h.run("run"); code != exitFailure || !strings.Contains(h.stderr.String(), "no accounts registered") {
		t.Errorf("no accounts: code=%d stderr=%s", code, h.stderr.String())
	}
	h.stderr.Reset()
	h.addAccounts(t, "a", "b")
	if code := h.run("run", "--account", "ghost"); code != exitFailure || !strings.Contains(h.stderr.String(), "not registered") {
		t.Errorf("unknown: code=%d stderr=%s", code, h.stderr.String())
	}
	if n := len(h.starter().snapshot()); n != 0 {
		t.Errorf("launched %d times for an unknown account", n)
	}
	// With no explicit account and no history, the first registered account is used.
	if code := h.run("run", "--", "--version"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	if got := cfgDir(h.starter().snapshot()[0]); !strings.HasSuffix(got, "/profiles/a") {
		t.Errorf("default pick launched with %q", got)
	}
	if code := h.run("run", "--account", "b", "--", "--version"); code != exitOK {
		t.Errorf("explicit account: code=%d", code)
	}
	if got := cfgDir(h.starter().snapshot()[1]); !strings.HasSuffix(got, "/profiles/b") {
		t.Errorf("launched with %q", got)
	}
}

func TestRunRemembersLastActiveAccount(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a", "b")
	h.run("run", "--account", "b")
	h.run("run")
	specs := h.starter().snapshot()
	if len(specs) != 2 || !strings.HasSuffix(cfgDir(specs[1]), "/profiles/b") {
		t.Errorf("second run should reuse b: %v", specs)
	}
}

func TestRunSkipsExhaustedAccountAndSaysSoWhenAllAre(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a", "b")
	h.writeState(t, "state.json", `{"version":1,"active":"a","accounts":{"a":{"status":"exhausted","until":"2026-10-20T15:00:00Z","since":"2026-10-20T10:00:00Z"}}}`)
	h.run("run")
	if got := cfgDir(h.starter().snapshot()[0]); !strings.HasSuffix(got, "/profiles/b") {
		t.Errorf("should move off exhausted a, got %q", got)
	}
	h.writeState(t, "state.json", `{"version":1,"active":"a","accounts":{`+
		`"a":{"status":"exhausted","until":"2026-10-20T18:00:00Z","since":"2026-10-20T10:00:00Z"},`+
		`"b":{"status":"exhausted","until":"2026-10-20T15:00:00Z","since":"2026-10-20T10:00:00Z"}}}`)
	h.stderr.Reset()
	h.run("run")
	if got := cfgDir(h.starter().snapshot()[1]); !strings.HasSuffix(got, "/profiles/b") {
		t.Errorf("all exhausted: should start on the earliest reset (b), got %q", got)
	}
	if !strings.Contains(h.stderr.String(), "All accounts are at their limit") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestRunRefusesNesting(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a")
	base := h.env.Getenv
	h.env.Getenv = func(k string) string {
		if k == "CLAUDE_GO_WRAPPED" {
			return "123"
		}
		return base(k)
	}
	if code := h.run("run"); code != exitFailure || !strings.Contains(h.stderr.String(), "nested") {
		t.Errorf("code=%d stderr=%s", code, h.stderr.String())
	}
	if n := len(h.starter().snapshot()); n != 0 {
		t.Errorf("nested run launched a child")
	}
}

func TestRunFailsWithoutClaude(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a")
	h.env.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if code := h.run("run"); code != exitFailure || !strings.Contains(h.stderr.String(), "claude executable not found") {
		t.Errorf("code=%d stderr=%s", code, h.stderr.String())
	}
}

func TestRunRefusesProfileWithOwnHistory(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a")
	proj := filepath.Join(h.home, "profiles", "a", "projects")
	if err := os.Remove(proj); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := h.run("run"); code != exitFailure || !strings.Contains(h.stderr.String(), "conversation history") {
		t.Errorf("code=%d stderr=%s", code, h.stderr.String())
	}
	if n := len(h.starter().snapshot()); n != 0 {
		t.Error("launched despite unresolved history conflict")
	}
}

func TestHandoffWithNoSession(t *testing.T) {
	h := newHarness(t)
	if code := h.run("handoff", "b"); code != exitFailure || !strings.Contains(h.stderr.String(), "no running claude-go session") {
		t.Errorf("code=%d stderr=%s", code, h.stderr.String())
	}
	if code := h.run("handoff"); code != exitUsage {
		t.Errorf("missing account: code=%d", code)
	}
}

func loggedInRunner(loggedIn map[string]bool) fakeRunner {
	return fakeRunner{fn: func(env []string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "auth" {
			for _, kv := range env {
				if dir, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
					if loggedIn[filepath.Base(dir)] {
						return []byte(`{"loggedIn":true}`), nil
					}
				}
			}
			return []byte(`{"loggedIn":false}`), nil
		}
		return []byte("9.9.9 (Claude Code)\n"), nil
	}}
}

func TestRunThenHandoffEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a", "b", "c")
	h.env.Runner = loggedInRunner(map[string]bool{"b": true})
	st := h.starter()
	st.block = true

	// The harness shares one stdout buffer; give the long-running session its own streams.
	sessEnv := h.env
	sessEnv.Stdout, sessEnv.Stderr = &strings.Builder{}, &strings.Builder{}
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), []string{"run", "--account", "a"}, sessEnv) }()
	waitUntil(t, func() bool { return len(st.snapshot()) == 1 })

	// Unregistered and logged-out targets are rejected without touching the child.
	for _, target := range []string{"ghost", "c"} {
		if code := h.run("handoff", target); code != exitFailure {
			t.Errorf("handoff to %s should fail, code=%d", target, code)
		}
	}
	if n := len(st.snapshot()); n != 1 {
		t.Fatalf("rejected handoff launched a child (%d launches)", n)
	}

	h.stdout.Reset()
	if code := h.run("handoff", "b"); code != exitOK {
		t.Fatalf("handoff: code=%d stderr=%s", code, h.stderr.String())
	}
	specs := st.snapshot()
	if len(specs) != 2 {
		t.Fatalf("launches = %d", len(specs))
	}
	sid := specs[0].Args[1]
	if !reflect.DeepEqual(specs[1].Args, []string{"--resume", sid}) && !reflect.DeepEqual(specs[1].Args, []string{"--session-id", sid}) {
		t.Errorf("second launch args = %v", specs[1].Args)
	}
	if !strings.HasSuffix(cfgDir(specs[1]), "/profiles/b") {
		t.Errorf("second launch config dir = %q", cfgDir(specs[1]))
	}
	if !strings.Contains(h.stdout.String(), sid) {
		t.Errorf("handoff output should name the session: %q", h.stdout.String())
	}

	st.mu.Lock()
	st.procs[1].exit <- 0
	st.mu.Unlock()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("session exit = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
