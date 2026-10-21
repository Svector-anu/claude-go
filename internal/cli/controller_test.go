package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-go/internal/availability"
	"claude-go/internal/ipc"
	"claude-go/internal/launcher"
)

// newShortHarness keeps the state directory short enough for a Unix socket path.
func newShortHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	dir, err := os.MkdirTemp("", "cg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h.home = dir
	h.vars["CLAUDE_GO_HOME"] = dir
	return h
}

// sessionLaunches returns the Claude session launches, leaving out sign-in runs.
func sessionLaunches(h *harness) []launcher.Spec {
	var out []launcher.Spec
	for _, sp := range h.starter().snapshot() {
		if len(sp.Args) > 0 && sp.Args[0] == "auth" {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// liveSession starts `claude-go run` for account a and returns an IPC client
// for it plus a function that ends the session.
func liveSession(t *testing.T, h *harness, args ...string) (*ipc.Client, func()) {
	t.Helper()
	h.starter().block = true
	sessEnv := h.env
	sessEnv.Stdout, sessEnv.Stderr = &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), append([]string{"run"}, args...), sessEnv) }()
	waitUntil(t, func() bool { return len(sessionLaunches(h)) == 1 })

	spec := sessionLaunches(h)[0]
	var sock string
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_GO_SOCKET="); ok {
			sock = v
		}
	}
	if sock == "" {
		t.Fatalf("the child was not given CLAUDE_GO_SOCKET; env=%v", spec.Env)
	}
	end := func() {
		h.starter().mu.Lock()
		for _, p := range h.starter().procs {
			select {
			case p.exit <- 0:
			default:
			}
		}
		h.starter().mu.Unlock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("session did not end")
		}
	}
	return ipc.NewClient(sock), end
}

func TestModTalksToTheRunningController(t *testing.T) {
	h := newShortHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("accounts", "add", "b", "--login")
	cl, end := liveSession(t, h, "--account", "a")
	defer end()
	ctx := context.Background()

	health, err := cl.Health(ctx)
	if err != nil || !health.OK || health.APIVersion != ipc.APIVersion {
		t.Fatalf("health = %+v, %v", health, err)
	}
	st, err := cl.Status(ctx)
	if err != nil || st.Session.Account.ID != "a" || st.Session.Phase != "running" || !st.Session.HandoffAvailable {
		t.Fatalf("status = %+v, %v", st, err)
	}
	accts, err := cl.Accounts(ctx)
	if err != nil || len(accts) != 2 || !accts[0].Current || accts[1].Current || accts[1].State != ipc.StateReady {
		t.Fatalf("accounts = %+v, %v", accts, err)
	}
	if strings.Contains(strings.ToLower(accts[0].Label+accts[1].Label), "token") {
		t.Error("labels must never carry credentials")
	}

	// Usage reported by the mod is attributed to the live account only.
	reset := h.env.Now().Add(2 * time.Hour)
	if err := cl.ReportUsage(ctx, []ipc.Window{{Kind: "five_hour", PercentUsed: 97, ResetsAt: &reset}}); err != nil {
		t.Fatal(err)
	}
	accts, _ = cl.Accounts(ctx)
	if accts[0].LastUsage == nil || accts[0].LastUsage.Windows[0].PercentUsed != 97 || accts[1].LastUsage != nil {
		t.Errorf("usage attribution wrong: %+v", accts)
	}

	// A manual handoff through the socket.
	acc, err := cl.Handoff(ctx, "req-manual01", "b")
	if err != nil || !acc.Accepted {
		t.Fatalf("handoff = %+v, %v", acc, err)
	}
	waitUntil(t, func() bool { return len(sessionLaunches(h)) == 2 })
	if got := cfgDir(sessionLaunches(h)[1]); !strings.HasSuffix(got, "/profiles/b") {
		t.Errorf("continued on %q", got)
	}
	waitUntil(t, func() bool {
		st, err := cl.Status(ctx)
		return err == nil && st.Session.Account.ID == "b" && st.LastHandoff != nil
	})
	st, _ = cl.Status(ctx)
	if st.LastHandoff.Result != "ok" || st.LastHandoff.Trigger != "manual" || st.LastHandoff.From != "a" || st.LastHandoff.To != "b" {
		t.Errorf("last handoff = %+v", st.LastHandoff)
	}
	hist, err := cl.History(ctx, 5)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

func TestSocketHandoffToUnknownOrSignedOutAccountIsRefusedAndSessionSurvives(t *testing.T) {
	h := newShortHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("accounts", "add", "b") // never signed in
	cl, end := liveSession(t, h, "--account", "a")
	defer end()
	ctx := context.Background()

	for _, target := range []string{"ghost", "b", "a"} {
		_, err := cl.Handoff(ctx, "req-"+target+"-0001", target)
		var ae *ipc.APIError
		if !errors.As(err, &ae) || ae.Status != 422 {
			t.Errorf("%s: err = %v", target, err)
		}
	}
	if n := len(sessionLaunches(h)); n != 1 {
		t.Errorf("launches = %d; refused requests must not restart Claude", n)
	}
	hist, _ := cl.History(ctx, 10)
	if len(hist) != 3 || hist[0].Result != "rejected" {
		t.Errorf("refusals should be recorded: %+v", hist)
	}
}

func TestSessionThatCannotResumeReportsWhySwitchingIsUnavailable(t *testing.T) {
	h := newShortHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("accounts", "add", "b", "--login")
	cl, end := liveSession(t, h, "--account", "a", "--", "--continue")
	defer end()
	st, err := cl.Status(context.Background())
	if err != nil || st.Session.HandoffAvailable || st.Session.HandoffBlock == "" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := cl.Handoff(context.Background(), "req-blocked1", "b"); err == nil {
		t.Error("handoff of an unresumable session must be refused")
	}
}

func TestAutoSwitchIsVisibleToTheModAndUsesVerifiedReset(t *testing.T) {
	h := newShortHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("accounts", "add", "b", "--login")
	cl, end := liveSession(t, h, "--account", "a")
	defer end()
	ctx := context.Background()

	// Claude reported a spent window with a precise reset time...
	reset := h.env.Now().Add(90 * time.Minute)
	if err := cl.ReportUsage(ctx, []ipc.Window{{Kind: "five_hour", PercentUsed: 99, ResetsAt: &reset}}); err != nil {
		t.Fatal(err)
	}
	// ...and then a limit failure arrives with no reset in its text.
	sid := sessionLaunches(h)[0].Args[1]
	writeTranscript(t, filepath.Join(h.userHome, ".claude", "projects", "-work", sid+".jsonl"))
	h.hook(t, itoa(os.Getpid()), `{"error":"rate_limit"}`)
	waitUntil(t, func() bool { return len(sessionLaunches(h)) == 2 })

	st, _ := availability.Load(filepath.Join(h.home, "state.json"))
	ea := st.Accounts["a"]
	if ea.Estimated || !ea.Until.Equal(reset.UTC()) {
		t.Errorf("reset should come from Claude's own figures, got %+v (want %v)", ea, reset)
	}
	waitUntil(t, func() bool {
		s, err := cl.Status(ctx)
		return err == nil && s.LastHandoff != nil && s.LastHandoff.Trigger == "auto" && s.Session.Account.ID == "b"
	})
	s, _ := cl.Status(ctx)
	if s.LastHandoff.Reason != "usage limit" || s.LastHandoff.Result != "ok" {
		t.Errorf("last handoff = %+v", s.LastHandoff)
	}
	accts, _ := cl.Accounts(ctx)
	if accts[0].State != ipc.StateLimit || accts[0].Until == nil || accts[0].Estimated {
		t.Errorf("account a view = %+v", accts[0])
	}
}

func TestStatusAttentionCarriesARecoveryAction(t *testing.T) {
	h := newShortHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	cl, end := liveSession(t, h, "--account", "a")
	defer end()
	h.hook(t, itoa(os.Getpid()), `{"error":"rate_limit","error_details":"limit reached"}`)
	waitUntil(t, func() bool {
		s, err := cl.Status(context.Background())
		return err == nil && s.Session.Attention != ""
	})
	s, _ := cl.Status(context.Background())
	if !strings.Contains(s.Session.Attention, "no other account") || !strings.Contains(s.Session.Action, "accounts add") {
		t.Errorf("attention=%q action=%q", s.Session.Attention, s.Session.Action)
	}
	hist, _ := cl.History(context.Background(), 5)
	if len(hist) != 1 || hist[0].Result != "unavailable" || hist[0].Trigger != "auto" {
		t.Errorf("history = %+v", hist)
	}
}

func TestLongStateDirectoryDegradesWithoutBreakingTheSession(t *testing.T) {
	h := newHarness(t) // t.TempDir paths are too long for a socket on macOS
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.starter().block = true
	before := len(h.starter().snapshot())
	sessEnv := h.env
	sessErr := &syncBuffer{}
	sessEnv.Stdout, sessEnv.Stderr = &syncBuffer{}, sessErr
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), []string{"run", "--account", "a"}, sessEnv) }()
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+1 })
	for _, kv := range h.starter().snapshot()[before].Env {
		if strings.HasPrefix(kv, "CLAUDE_GO_SOCKET=") && len(h.home) > 60 {
			t.Errorf("no socket should be advertised when none is served: %s", kv)
		}
	}
	if sessErr.String() != "" {
		t.Errorf("a missing socket must not print into the session: %q", sessErr.String())
	}
	h.starter().mu.Lock()
	h.starter().procs[before].exit <- 0
	h.starter().mu.Unlock()
	<-done
}

func listenAndHang(t *testing.T, path string) (stop func()) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}
}
