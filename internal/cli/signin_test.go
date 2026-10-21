package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Svector-anu/claude-go/internal/registry"
)

func TestAddWithLoginSignsInUnderTheNewProfileOnly(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"work": "work@example.com"})
	h.vars["CLAUDE_CONFIG_DIR"] = "/should/not/leak"
	h.env.Environ = func() []string { return []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/should/not/leak"} }

	if code := h.run("accounts", "add", "--login", "--email", "work@example.com", "work"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	specs := h.starter().snapshot()
	if len(specs) != 1 {
		t.Fatalf("launches = %d", len(specs))
	}
	want := filepath.Join(h.home, "profiles", "work")
	if got := strings.Join(specs[0].Args, " "); got != "auth login --email work@example.com" {
		t.Errorf("login args = %q", got)
	}
	if cfgDir(specs[0]) != want {
		t.Errorf("login ran with CLAUDE_CONFIG_DIR=%q, want %q", cfgDir(specs[0]), want)
	}
	count := 0
	for _, kv := range specs[0].Env {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR appears %d times", count)
	}

	reg, _ := registry.Load(filepath.Join(h.home, "accounts.json"))
	if len(reg.Accounts) != 1 || reg.Accounts[0].Email != "work@example.com" || reg.Accounts[0].Label != "work@example.com" || reg.Accounts[0].OrgID == "" {
		t.Errorf("registry = %+v", reg.Accounts)
	}
}

func TestAddWithLoginFailureLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	h.starter().exitCode = 1
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("accounts", "add", "--login", "work"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if reg, _ := registry.Load(filepath.Join(h.home, "accounts.json")); len(reg.Accounts) != 0 {
		t.Errorf("registered: %+v", reg.Accounts)
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "work")); err == nil {
		t.Error("profile left behind")
	}
}

func TestAddWithLoginThatDidNotCompleteIsRejected(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil) // profile stays logged out although the command exited 0
	if code := h.run("accounts", "add", "--login", "work"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "did not complete") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestAddWithLoginRefusesTheDefaultAccountsLogin(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"second": "me@example.com"}) // same login as the default
	h.run("setup", "--yes")
	h.stderr.Reset()
	if code := h.run("accounts", "add", "--login", "second"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(h.stderr.String(), "already registered") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	reg, _ := registry.Load(filepath.Join(h.home, "accounts.json"))
	if len(reg.Accounts) != 1 {
		t.Errorf("twin registered: %+v", reg.Accounts)
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "second")); err == nil {
		t.Error("twin profile left behind")
	}
}

func TestAccountsLoginForDefaultAccountDoesNotExportConfigDir(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes")
	if code := h.run("accounts", "login", "default"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	specs := h.starter().snapshot()
	if got := cfgDir(specs[len(specs)-1]); got != "" {
		t.Errorf("default account login must not set CLAUDE_CONFIG_DIR, got %q", got)
	}
}

func TestAccountsLoginClearsUnavailableState(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"b": "b@example.com"})
	h.run("accounts", "add", "--login", "b")
	h.writeState(t, "state.json", `{"version":1,"accounts":{"b":{"status":"auth_failed","since":"2026-10-20T10:00:00Z"}}}`)
	if code := h.run("accounts", "login", "b"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	h.stdout.Reset()
	h.run("accounts", "list")
	if !strings.Contains(h.stdout.String(), "ready") || strings.Contains(h.stdout.String(), "needs sign-in") {
		t.Errorf("list:\n%s", h.stdout.String())
	}
}

func TestAccountsEnable(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "a")
	h.writeState(t, "state.json", `{"version":1,"accounts":{"a":{"status":"exhausted","until":"2026-10-20T20:00:00Z","since":"2026-10-20T10:00:00Z"}}}`)
	h.run("accounts", "list")
	if !strings.Contains(h.stdout.String(), "at its limit until") {
		t.Fatalf("list:\n%s", h.stdout.String())
	}
	if code := h.run("accounts", "enable", "a"); code != exitOK {
		t.Fatal(code)
	}
	h.stdout.Reset()
	h.run("accounts", "list")
	if !strings.Contains(h.stdout.String(), "ready") {
		t.Errorf("list:\n%s", h.stdout.String())
	}
}

func TestRemovingDefaultAccountNeverTouchesClaudeDir(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	os.MkdirAll(filepath.Join(h.userHome, ".claude"), 0o700)
	marker := filepath.Join(h.userHome, ".claude", "precious")
	os.WriteFile(marker, []byte("x"), 0o600)
	h.run("setup", "--yes")
	if code := h.run("accounts", "remove", "--purge-profile", "default"); code != exitFailure {
		t.Errorf("purging the default account must fail, code=%d", code)
	}
	if code := h.run("accounts", "remove", "default"); code != exitOK {
		t.Errorf("code=%d", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("~/.claude was touched")
	}
}

func TestFlagsMayFollowTheAccountName(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"work": "work@example.com"})
	if code := h.run("accounts", "add", "work", "--login", "--email", "work@example.com"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	if got := strings.Join(h.starter().snapshot()[0].Args, " "); got != "auth login --email work@example.com" {
		t.Errorf("args = %q", got)
	}
	if code := h.run("accounts", "remove", "work", "--purge-profile"); code != exitOK {
		t.Errorf("remove: code=%d stderr=%s", code, h.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "work")); err == nil {
		t.Error("--purge-profile after the id was ignored")
	}
	if code := h.run("accounts", "add", "a", "b"); code != exitUsage {
		t.Errorf("two ids: code=%d", code)
	}
}
