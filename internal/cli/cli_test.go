package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/buildinfo"
)

type fakeRunner struct {
	out []byte
	err error
	fn  func(env []string, args []string) ([]byte, error)
}

func (f fakeRunner) Output(_ context.Context, env []string, _ string, args ...string) ([]byte, error) {
	if f.fn != nil {
		return f.fn(env, args)
	}
	return f.out, f.err
}

type harness struct {
	env      Env
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	home     string // CLAUDE_GO_HOME
	userHome string // fake $HOME, holds .claude and .local/bin
	vars     map[string]string
	binary   string // stand-in for the running claude-go executable
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	transcriptFlushWait = 150 * time.Millisecond
	h := &harness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, home: t.TempDir(), userHome: t.TempDir()}
	h.binary = filepath.Join(t.TempDir(), "claude-go")
	if err := os.WriteFile(h.binary, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.vars = map[string]string{"CLAUDE_GO_HOME": h.home, "SHELL": "/bin/zsh", "PATH": "/usr/bin"}
	h.env = Env{
		Stdin:      strings.NewReader(""),
		Stdout:     h.stdout,
		Stderr:     h.stderr,
		Getenv:     func(k string) string { return h.vars[k] },
		UserHome:   func() (string, error) { return h.userHome, nil },
		Executable: func() (string, error) { return h.binary, nil },
		LookPath:   func(string) (string, error) { return "/bin/claude", nil },
		Runner:     fakeRunner{out: []byte("9.9.9 (Claude Code)\n")},
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
		Getwd:      func() (string, error) { return "/work", nil },
		Environ:    func() []string { return []string{"PATH=/bin", "HOME=/h"} },
		Getpid:     os.Getpid,
		Starter:    &stubStarter{},
		Signals:    func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} },
	}
	return h
}

func (h *harness) run(args ...string) int {
	return Run(context.Background(), args, h.env)
}

func (h *harness) writeState(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.home, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoArgsIsUsageError(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != exitUsage {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "Commands:") {
		t.Errorf("stderr missing usage: %q", h.stderr.String())
	}
}

func TestHelpGoesToStdout(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		h := newHarness(t)
		if code := h.run(arg); code != exitOK {
			t.Errorf("%s: code = %d", arg, code)
		}
		if !strings.Contains(h.stdout.String(), "accounts list") || !strings.Contains(h.stdout.String(), "setup") {
			t.Errorf("%s: help incomplete: %q", arg, h.stdout.String())
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code := h.run("version"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if want := "claude-go " + buildinfo.Version; !strings.HasPrefix(h.stdout.String(), want) {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t)
	if code := h.run("frobnicate"); code != exitUsage {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestStatusFresh(t *testing.T) {
	h := newHarness(t)
	if code := h.run("status"); code != exitOK {
		t.Fatalf("code = %d stderr=%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{h.home, "0 registered", "/bin/claude (9.9.9 (Claude Code))", "using defaults", "not installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusClaudeMissingIsReportedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.env.LookPath = func(string) (string, error) { return "", errors.New("nope") }
	if code := h.run("status"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "not found") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestStatusVersionFailureIsReportedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = fakeRunner{err: errors.New("exec failed")}
	if code := h.run("status"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "version unavailable") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestStatusCorruptStateFails(t *testing.T) {
	for _, file := range []string{"config.json", "accounts.json"} {
		h := newHarness(t)
		h.writeState(t, file, "{not json")
		if code := h.run("status"); code != exitFailure {
			t.Errorf("%s: code = %d", file, code)
		}
		if !strings.Contains(h.stderr.String(), file) {
			t.Errorf("%s: stderr should name the file: %q", file, h.stderr.String())
		}
	}
}

func TestStatusRejectsExtraArgs(t *testing.T) {
	h := newHarness(t)
	if code := h.run("status", "--wat"); code != exitUsage {
		t.Errorf("code = %d", code)
	}
}

func TestAccountsListEmpty(t *testing.T) {
	h := newHarness(t)
	if code := h.run("accounts", "list"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "No accounts registered") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestAccountsListPopulated(t *testing.T) {
	h := newHarness(t)
	h.writeState(t, "accounts.json", `{"version":1,"accounts":[{"id":"work","label":"Work","email":"w@example.com","created_at":"2026-10-20T00:00:00Z"}]}`)
	if code := h.run("accounts", "list"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	out := h.stdout.String()
	for _, want := range []string{"ACCOUNT", "Work (work)", "STATE", "LIMITS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAccountsListCorruptFails(t *testing.T) {
	h := newHarness(t)
	h.writeState(t, "accounts.json", "garbage")
	if code := h.run("accounts", "list"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "accounts.json") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestAccountsUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"accounts"}, {"accounts", "add"}, {"accounts", "bogus"}, {"accounts", "add", "a", "b"}, {"accounts", "remove"}, {"accounts", "list", "x"}} {
		h := newHarness(t)
		if code := h.run(args...); code != exitUsage {
			t.Errorf("%v: code = %d", args, code)
		}
	}
}

func TestAccountsAddListRemoveFlow(t *testing.T) {
	h := newHarness(t)
	if code := h.run("accounts", "add", "--label", "Work", "--email", "w@example.com", "work"); code != exitOK {
		t.Fatalf("add: code=%d stderr=%s", code, h.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "work")); err != nil {
		t.Fatalf("profile dir missing: %v", err)
	}
	if !strings.Contains(h.stdout.String(), "accounts login work") {
		t.Errorf("add without --login should say how to sign in: %q", h.stdout.String())
	}
	h.stdout.Reset()
	if code := h.run("accounts", "list"); code != exitOK || !strings.Contains(h.stdout.String(), "Work") {
		t.Fatalf("list: code=%d out=%s", code, h.stdout.String())
	}
	if code := h.run("accounts", "add", "work"); code != exitFailure {
		t.Errorf("duplicate add code = %d", code)
	}

	h.stdout.Reset()
	if code := h.run("accounts", "remove", "work"); code != exitOK || !strings.Contains(h.stdout.String(), "kept") {
		t.Fatalf("remove: code=%d out=%s", code, h.stdout.String())
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "work")); err != nil {
		t.Error("profile deleted without --purge-profile")
	}
	if code := h.run("accounts", "remove", "--purge-profile", "work"); code != exitFailure {
		t.Errorf("removing an unregistered account should fail, code = %d", code)
	}
}

func TestAccountsAddRejectsBadID(t *testing.T) {
	h := newHarness(t)
	if code := h.run("accounts", "add", "../x"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "invalid account id") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestAccountsAddOnCorruptRegistryFails(t *testing.T) {
	h := newHarness(t)
	h.writeState(t, "accounts.json", "garbage")
	if code := h.run("accounts", "add", "a"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if got, _ := os.ReadFile(filepath.Join(h.home, "accounts.json")); string(got) != "garbage" {
		t.Error("corrupt registry overwritten")
	}
}
