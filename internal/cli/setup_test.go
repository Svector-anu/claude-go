package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Svector-anu/claude-go/internal/registry"
)

// identityRunner answers `claude auth status` per profile; unknown profiles are logged out.
// The default login (no CLAUDE_CONFIG_DIR) is "me@example.com".
func identityRunner(h *harness, byProfile map[string]string) fakeRunner {
	return fakeRunner{fn: func(env []string, args []string) ([]byte, error) {
		if len(args) == 0 || args[0] != "auth" {
			return []byte("9.9.9 (Claude Code)\n"), nil
		}
		dir := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
				dir = v
			}
		}
		email := ""
		if dir == "" {
			email = "me@example.com"
		} else {
			email = byProfile[filepath.Base(dir)]
		}
		if email == "" {
			return []byte(`{"loggedIn":false}`), nil
		}
		out, _ := json.Marshal(map[string]any{
			"loggedIn": true, "email": email, "orgId": "org-" + email,
			"configDirectory": firstNonEmpty(dir, filepath.Join(h.userHome, ".claude")),
		})
		return out, nil
	}}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *harness) settingsPath() string { return filepath.Join(h.userHome, ".claude", "settings.json") }
func (h *harness) zshrc() string        { return filepath.Join(h.userHome, ".zshrc") }
func (h *harness) installedBin() string {
	return filepath.Join(h.userHome, ".local", "bin", "claude-go")
}

func TestSetupYesDoesEverythingOnce(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("code=%d\nstdout=%s\nstderr=%s", code, h.stdout.String(), h.stderr.String())
	}

	reg, err := registry.Load(filepath.Join(h.home, "accounts.json"))
	if err != nil || len(reg.Accounts) != 1 {
		t.Fatalf("registry: %+v %v", reg, err)
	}
	def := reg.Accounts[0]
	if !def.Default || def.ID != "default" || def.Email != "me@example.com" || def.ConfigDir != filepath.Join(h.userHome, ".claude") {
		t.Errorf("default account = %+v", def)
	}

	fi, err := os.Stat(h.installedBin())
	if err != nil || fi.Mode().Perm() != 0o755 || readFile(t, h.installedBin()) != "fake binary" {
		t.Errorf("binary not installed correctly: %v", err)
	}
	if hook := readFile(t, h.settingsPath()); !strings.Contains(hook, h.installedBin()+" hook stop-failure") || !strings.Contains(hook, "StopFailure") {
		t.Errorf("settings.json = %s", hook)
	}
	rc := readFile(t, h.zshrc())
	if !strings.Contains(rc, `export PATH="$HOME/.local/bin:$PATH"`) || !strings.Contains(rc, "alias claude='\""+h.installedBin()+"\" run'") {
		t.Errorf(".zshrc = %s", rc)
	}
	if !strings.Contains(h.stdout.String(), "accounts add personal --login") {
		t.Errorf("should point at the next step:\n%s", h.stdout.String())
	}

	// Running it again changes nothing.
	beforeRC, beforeSettings := rc, readFile(t, h.settingsPath())
	h.stdout.Reset()
	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("second run code=%d", code)
	}
	if readFile(t, h.zshrc()) != beforeRC || readFile(t, h.settingsPath()) != beforeSettings {
		t.Error("second setup modified files")
	}
	if reg2, _ := registry.Load(filepath.Join(h.home, "accounts.json")); len(reg2.Accounts) != 1 {
		t.Errorf("second setup duplicated the account: %+v", reg2.Accounts)
	}
}

func TestSetupNonInteractiveSkipsOptionalSteps(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	if _, err := os.Stat(h.installedBin()); err == nil {
		t.Error("binary installed without consent")
	}
	if _, err := os.Stat(h.zshrc()); err == nil {
		t.Error("shell config edited without consent")
	}
	if hook := readFile(t, h.settingsPath()); !strings.Contains(hook, h.binary+" hook stop-failure") {
		t.Errorf("hook should point at the running binary: %s", hook)
	}
}

func TestSetupAsksOneQuestionUpFrontAndExplainsWhatItWillDo(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.env.Interactive = true
	h.env.Stdin = strings.NewReader("y\n")
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if strings.Count(out, "[Y/n]") != 1 {
		t.Errorf("expected exactly one question:\n%s", out)
	}
	if !strings.Contains(out, "This will:") || strings.Index(out, "This will:") > strings.Index(out, "Continue?") {
		t.Errorf("the plan must come before the question:\n%s", out)
	}
	if strings.Index(out, "Continue?") > strings.Index(out, "✓") {
		t.Errorf("no result may be printed before the question is answered:\n%s", out)
	}
	for _, want := range []string{"Your login", "Command", "Limit hook", "Status line", "Companion", "Shortcut"} {
		if !strings.Contains(out, "✓ "+want) {
			t.Errorf("missing result row %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, h.userHome) {
		t.Errorf("paths should be shortened with ~:\n%s", out)
	}
	if _, err := os.Stat(h.installedBin()); err != nil {
		t.Error("binary should have been installed")
	}
	rc := readFile(t, h.zshrc())
	if !strings.Contains(rc, "alias claude=") || !strings.Contains(rc, "export PATH") {
		t.Errorf(".zshrc = %s", rc)
	}
}

func TestSetupDeclinedChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.env.Interactive = true
	h.env.Stdin = strings.NewReader("n\n")
	if code := h.run("setup"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(h.stdout.String(), "Nothing changed.") {
		t.Errorf("out = %s", h.stdout.String())
	}
	for _, p := range []string{h.settingsPath(), h.zshrc(), h.installedBin(), filepath.Join(h.home, "accounts.json")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created after declining", p)
		}
	}
}

func TestSetupOptOutsAreHonoured(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("setup", "--yes", "--no-alias", "--no-install", "--no-mod", "--no-statusline"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(h.zshrc()); err == nil {
		t.Error("--no-alias still edited the shell config")
	}
	if _, err := os.Stat(h.installedBin()); err == nil {
		t.Error("--no-install still copied the binary")
	}
	if !strings.Contains(readFile(t, h.settingsPath()), h.binary+" hook stop-failure") {
		t.Error("the hook must still point at the running binary")
	}
}

func TestSetupWhenNotSignedIn(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = fakeRunner{out: []byte(`{"loggedIn":false}`)}
	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if reg, _ := registry.Load(filepath.Join(h.home, "accounts.json")); len(reg.Accounts) != 0 {
		t.Errorf("registered an account without a login: %+v", reg.Accounts)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "claude auth login") || !strings.Contains(out, "✓") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetupRefusesInvalidSettingsAndLeavesThemAlone(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	if err := os.MkdirAll(filepath.Dir(h.settingsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.settingsPath(), []byte(`{"model": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("setup", "--yes"); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if readFile(t, h.settingsPath()) != `{"model": ` {
		t.Error("invalid settings.json was modified")
	}
}

func TestSetupWarnsAboutProfileWithOwnSettingsAndTwinLogins(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "twin")
	profile := filepath.Join(h.home, "profiles", "twin")
	if err := os.MkdirAll(filepath.Join(h.userHome, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(h.settingsPath(), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{}`), 0o600)
	h.env.Runner = identityRunner(h, map[string]string{"twin": "me@example.com"})

	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, `has its own settings.json`) {
		t.Errorf("missing settings warning:\n%s", out)
	}
	if !strings.Contains(out, "are the same login") {
		t.Errorf("missing twin warning:\n%s", out)
	}
	if got := readFile(t, filepath.Join(profile, "settings.json")); got != `{}` {
		t.Errorf("profile settings.json was overwritten: %q", got)
	}
}

func TestSetupLinksSettingsIntoExistingProfiles(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "b")
	os.MkdirAll(filepath.Join(h.userHome, ".claude"), 0o700)
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	link := filepath.Join(h.home, "profiles", "b", "settings.json")
	target, err := os.Readlink(link)
	if err != nil || target != h.settingsPath() {
		t.Errorf("settings link = %q, %v", target, err)
	}
}

func TestSetupUndoRemovesHookAndAliasButKeepsAccounts(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes")
	h.stdout.Reset()
	if code := h.run("setup", "--undo"); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, h.stderr.String())
	}
	if strings.Contains(readFile(t, h.settingsPath()), "claude-go") {
		t.Error("hook still present")
	}
	if strings.Contains(readFile(t, h.zshrc()), "claude-go") {
		t.Error("shell block still present")
	}
	if reg, _ := registry.Load(filepath.Join(h.home, "accounts.json")); len(reg.Accounts) != 1 {
		t.Error("undo must keep accounts")
	}
}

func TestStatusShowsHookAndAccountState(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes")
	h.writeState(t, "state.json", `{"version":1,"accounts":{"default":{"status":"auth_failed","since":"2026-10-20T10:00:00Z"}}}`)
	h.stdout.Reset()
	if code := h.run("status"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "installed in") || !strings.Contains(out, "needs sign-in") {
		t.Errorf("status:\n%s", out)
	}
}

func TestSetupCarriesOnboardingIntoExistingProfiles(t *testing.T) {
	h := newHarness(t)
	h.addAccounts(t, "b")
	os.WriteFile(filepath.Join(h.userHome, ".claude.json"), []byte(`{"hasCompletedOnboarding":true,"projects":{"/w":{"hasTrustDialogAccepted":true}}}`), 0o600)
	h.env.Runner = identityRunner(h, nil)
	if code := h.run("setup", "--yes"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(h.stdout.String(), "onboarding and trusted folders carried over") {
		t.Errorf("setup output:\n%s", h.stdout.String())
	}
	state := readFile(t, filepath.Join(h.home, "profiles", "b", ".claude.json"))
	if !strings.Contains(state, "hasCompletedOnboarding") || !strings.Contains(state, "hasTrustDialogAccepted") {
		t.Errorf("profile state = %s", state)
	}
}

func TestEveryLaunchAndHandoffTargetIsSeededBeforeClaudeStarts(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	os.WriteFile(filepath.Join(h.userHome, ".claude.json"), []byte(`{"hasCompletedOnboarding":true}`), 0o600)
	h.run("run", "--account", "a")
	state := readFile(t, filepath.Join(h.home, "profiles", "a", ".claude.json"))
	if !strings.Contains(state, "hasCompletedOnboarding") {
		t.Errorf("the profile was not seeded before its Claude launched: %s", state)
	}
}
