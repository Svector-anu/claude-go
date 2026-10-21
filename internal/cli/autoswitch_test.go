package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/availability"
)

// syncBuffer lets the test read what the session goroutine wrote.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestLimitHookSwitchesAccountEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "--login", "a")
	h.run("accounts", "add", "--login", "b")
	h.starter().block = true
	h.starter().exitCode = 0
	before := len(h.starter().snapshot())

	sessEnv := h.env
	sessOut, sessErr := &syncBuffer{}, &syncBuffer{}
	sessEnv.Stdout, sessEnv.Stderr = sessOut, sessErr
	done := make(chan int, 1)
	go func() {
		done <- Run(context.Background(), []string{"run", "--account", "a", "--model", "opus", "do the thing"}, sessEnv)
	}()
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+1 })
	first := h.starter().snapshot()[before]
	sid := first.Args[1]
	// Claude has written the conversation by now; the shared store is ~/.claude/projects.
	writeTranscript(t, filepath.Join(h.userHome, ".claude", "projects", "-work", sid+".jsonl"))

	// Claude reports that account a ran out; the hook is exactly what Claude would invoke.
	limit := `{"session_id":"` + sid + `","error":"rate_limit","error_details":"5-hour limit reached ∙ resets 3pm (America/New_York)"}`
	if code := h.hook(t, itoa(sessEnv.Getpid()), limit); code != exitOK {
		t.Fatalf("hook code = %d", code)
	}
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+2 })

	second := h.starter().snapshot()[before+1]
	if want := []string{"--resume", sid, "--model", "opus"}; !reflect.DeepEqual(second.Args, want) {
		t.Errorf("resumed with %v, want %v", second.Args, want)
	}
	if want := filepath.Join(h.home, "profiles", "b"); cfgDir(second) != want {
		t.Errorf("continued on %q, want %q", cfgDir(second), want)
	}
	if got, want := sessErr.String(), "Account a@example.com hit its limit, continuing on Account b@example.com\n"; got != want {
		t.Errorf("terminal output = %q, want exactly %q", got, want)
	}

	st, err := availability.Load(filepath.Join(h.home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ea := st.Accounts["a"]
	if ea.Status != availability.Exhausted || ea.Estimated || ea.Until.UTC().Hour() != 19 {
		t.Errorf("account a state = %+v", ea)
	}
	waitUntil(t, func() bool {
		st, _ := availability.Load(filepath.Join(h.home, "state.json"))
		return st.Active == "b"
	})

	// Now b runs out too: nobody is left, so the session keeps running and explains at exit.
	if code := h.hook(t, itoa(sessEnv.Getpid()), `{"session_id":"`+sid+`","error":"rate_limit","error_details":"resets 6pm (America/New_York)"}`); code != exitOK {
		t.Fatalf("hook code = %d", code)
	}
	waitUntil(t, func() bool {
		st, _ := availability.Load(filepath.Join(h.home, "state.json"))
		return st.Accounts["b"].Status == availability.Exhausted
	})
	time.Sleep(60 * time.Millisecond)
	if n := len(h.starter().snapshot()); n != before+2 {
		t.Fatalf("launched %d children; nothing is available so none should be added", n-before)
	}

	h.starter().mu.Lock()
	h.starter().procs[before+1].exit <- 0
	h.starter().mu.Unlock()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session did not end")
	}
	out := sessErr.String()
	if !strings.Contains(out, "no other account is available") || !strings.Contains(out, "Earliest reset") {
		t.Errorf("exit explanation missing:\n%s", out)
	}
	if strings.Count(out, "continuing on") != 1 {
		t.Errorf("exactly one switch line expected:\n%s", out)
	}
}

func writeTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthFailureSwitchesWithAnAccurateMessage(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "b": "b@example.com"})
	h.run("accounts", "add", "--login", "a")
	h.run("accounts", "add", "--login", "b")
	h.starter().block = true
	before := len(h.starter().snapshot())

	sessEnv := h.env
	sessErr := &syncBuffer{}
	sessEnv.Stdout, sessEnv.Stderr = &syncBuffer{}, sessErr
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), []string{"run", "--account", "a"}, sessEnv) }()
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+1 })

	h.hook(t, itoa(sessEnv.Getpid()), `{"error":"authentication_failed"}`)
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+2 })

	if got := sessErr.String(); got != "Account a@example.com needs to sign in again, continuing on Account b@example.com\n" {
		t.Errorf("output = %q", got)
	}
	st, _ := availability.Load(filepath.Join(h.home, "state.json"))
	if st.Accounts["a"].Status != availability.AuthFailed {
		t.Errorf("state = %+v", st.Accounts["a"])
	}
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
		t.Fatal("session did not end")
	}
}

func TestSwitchSkipsATargetThatIsNotSignedIn(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com", "c": "c@example.com"})
	h.run("accounts", "add", "--login", "a")
	h.run("accounts", "add", "b") // registered but never signed in
	h.run("accounts", "add", "--login", "c")
	h.starter().block = true
	before := len(h.starter().snapshot())

	sessEnv := h.env
	sessErr := &syncBuffer{}
	sessEnv.Stdout, sessEnv.Stderr = &syncBuffer{}, sessErr
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), []string{"run", "--account", "a"}, sessEnv) }()
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+1 })

	h.hook(t, itoa(sessEnv.Getpid()), `{"error":"rate_limit","error_details":"limit reached"}`)
	waitUntil(t, func() bool { return len(h.starter().snapshot()) == before+2 })

	if want := filepath.Join(h.home, "profiles", "c"); cfgDir(h.starter().snapshot()[before+1]) != want {
		t.Errorf("should have skipped the signed-out account b and used c")
	}
	st, _ := availability.Load(filepath.Join(h.home, "state.json"))
	if st.Accounts["b"].Status != availability.AuthFailed {
		t.Errorf("b should be marked as needing sign-in: %+v", st.Accounts["b"])
	}
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
		t.Fatal("session did not end")
	}
}
