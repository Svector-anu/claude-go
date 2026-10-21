package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (h *harness) modDir() string { return filepath.Join(h.home, "mod", "account-flow") }

func envOf(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			val, n = v, n+1
		}
	}
	return val, n
}

func TestSetupModInstallsIntoClaudeGoStateOnly(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	os.MkdirAll(filepath.Dir(h.settingsPath()), 0o700)
	os.WriteFile(h.settingsPath(), []byte(`{"enabledPlugins":{"x@y":true}}`), 0o600)
	pluginsDir := filepath.Join(h.userHome, ".claude", "plugins")
	os.MkdirAll(pluginsDir, 0o700)

	if code := h.run("setup", "--yes", "--mod"); code != exitOK {
		t.Fatalf("code=%d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	for _, want := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.tsx", "hooks/format.ts", "hooks/api.ts"} {
		if _, err := os.Stat(filepath.Join(h.modDir(), want)); err != nil {
			t.Errorf("mod file missing: %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join(h.modDir(), "tests")); err == nil {
		t.Error("tests must not be installed")
	}
	settings := readFile(t, h.settingsPath())
	if !strings.Contains(settings, `"x@y"`) || strings.Contains(settings, "account-flow") {
		t.Errorf("the mod must not be registered in Claude's settings: %s", settings)
	}
	if entries, _ := os.ReadDir(pluginsDir); len(entries) != 0 {
		t.Errorf("Claude's plugin store was touched: %v", entries)
	}
	h.stdout.Reset()
	h.run("status")
	if !strings.Contains(h.stdout.String(), "installed; loads in sessions started by claude-go") {
		t.Errorf("status:\n%s", h.stdout.String())
	}
}

func TestRunLoadsTheModOnlyWhenInstalled(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("run", "--account", "a")
	if v, n := envOf(h.starter().snapshot()[len(h.starter().snapshot())-1].Env, "CLAUDE_CODE_PLUGIN_DIRS"); n != 0 {
		t.Errorf("mod not installed, yet CLAUDE_CODE_PLUGIN_DIRS=%q", v)
	}

	h.run("setup", "--yes", "--mod")
	h.run("run", "--account", "a")
	sp := h.starter().snapshot()[len(h.starter().snapshot())-1]
	if v, n := envOf(sp.Env, "CLAUDE_CODE_PLUGIN_DIRS"); n != 1 || v != h.modDir() {
		t.Errorf("CLAUDE_CODE_PLUGIN_DIRS=%q x%d, want %q", v, n, h.modDir())
	}
}

func TestRunKeepsPluginFoldersTheUserAlreadyConfigured(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("setup", "--yes", "--mod")
	h.env.Environ = func() []string { return []string{"PATH=/bin", "CLAUDE_CODE_PLUGIN_DIRS=/their/plugin"} }
	h.run("run", "--account", "a")
	sp := h.starter().snapshot()[len(h.starter().snapshot())-1]
	v, n := envOf(sp.Env, "CLAUDE_CODE_PLUGIN_DIRS")
	if n != 1 || v != "/their/plugin"+string(os.PathListSeparator)+h.modDir() {
		t.Errorf("CLAUDE_CODE_PLUGIN_DIRS=%q x%d", v, n)
	}
}

func TestRunUpgradesAStaleInstalledMod(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("setup", "--yes", "--mod")
	manifest := filepath.Join(h.modDir(), ".claude-plugin", "plugin.json")
	os.WriteFile(manifest, []byte(`{"name":"account-flow","version":"0.0.1"}`), 0o644)
	os.WriteFile(filepath.Join(h.modDir(), "hooks", "leftover.ts"), []byte("old"), 0o644)

	h.run("run", "--account", "a")
	if got := readFile(t, manifest); strings.Contains(got, "0.0.1") {
		t.Errorf("stale mod was not refreshed: %s", got)
	}
	if _, err := os.Stat(filepath.Join(h.modDir(), "hooks", "leftover.ts")); err == nil {
		t.Error("old files survived the upgrade")
	}
}

func TestUndoRemovesTheModAndNothingElse(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes", "--mod")
	h.run("accounts", "add", "keep")
	if code := h.run("setup", "--undo"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(h.modDir()); err == nil {
		t.Error("mod still installed")
	}
	if _, err := os.Stat(filepath.Join(h.home, "profiles", "keep")); err != nil {
		t.Error("undo must keep profiles")
	}
}

func TestSetupNoModOptOutAndNonInteractiveInstallNoMod(t *testing.T) {
	for _, args := range [][]string{{"setup", "--yes", "--no-mod"}, {"setup"}} {
		h := newHarness(t)
		h.env.Runner = identityRunner(h, nil)
		h.run(args...)
		if _, err := os.Stat(h.modDir()); err == nil {
			t.Errorf("%v: mod installed", args)
		}
		h.stdout.Reset()
		h.run("status")
		if !strings.Contains(h.stdout.String(), "claude-go setup --mod") {
			t.Errorf("%v: status should say how to opt in:\n%s", args, h.stdout.String())
		}
	}
}

func TestSetupYesInstallsTheCompanionByDefault(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes")
	if _, err := os.Stat(h.modDir()); err != nil {
		t.Error("the recommended setup should include the companion")
	}
}

func TestPassthroughLaunchesDoNotLoadTheMod(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("setup", "--yes", "--mod")
	for _, args := range [][]string{{"run", "--account", "a", "--", "--version"}, {"run", "--account", "a", "--", "mcp", "list"}, {"run", "--account", "a", "--", "-p", "hi"}} {
		h.run(args...)
		sp := h.starter().snapshot()[len(h.starter().snapshot())-1]
		if v, n := envOf(sp.Env, "CLAUDE_CODE_PLUGIN_DIRS"); n != 0 {
			t.Errorf("%v: passthrough loaded the mod (%q)", args, v)
		}
		if _, n := envOf(sp.Env, "CLAUDE_GO_SOCKET"); n != 0 {
			t.Errorf("%v: passthrough was given a socket", args)
		}
	}
}
