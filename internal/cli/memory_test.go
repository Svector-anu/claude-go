package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Svector-anu/claude-go/internal/memory"
)

func (h *harness) gitProject(t *testing.T, remote string) string {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644)
	h.env.Getwd = func() (string, error) { return root, nil }
	return root
}

func (h *harness) memDir() string { return filepath.Join(h.home, "memory") }

func (h *harness) store(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.Open(h.memDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetupMemoryDeclaresTheServerInsideClaudeGosOwnMod(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	os.MkdirAll(filepath.Join(h.userHome, ".claude", "plugins"), 0o700)
	os.WriteFile(h.settingsPath(), []byte(`{"enabledPlugins":{"x@y":true}}`), 0o600)
	if code := h.run("setup", "--yes", "--memory"); code != exitOK {
		t.Fatalf("code=%d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	decl := readFile(t, filepath.Join(h.modDir(), ".mcp.json"))
	if !strings.Contains(decl, h.installedBin()) || !strings.Contains(decl, `"mcp-memory"`) {
		t.Errorf(".mcp.json = %s", decl)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(h.home, "memory.enabled"))); got != h.installedBin() {
		t.Errorf("marker = %q", got)
	}
	if s := readFile(t, h.settingsPath()); strings.Contains(s, "memory") || !strings.Contains(s, `"x@y"`) {
		t.Errorf("Claude's settings were touched: %s", s)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.userHome, ".claude", "plugins")); len(entries) != 0 {
		t.Errorf("Claude's plugin store was touched: %v", entries)
	}
	if !strings.Contains(h.stdout.String(), "✓ Memory") {
		t.Errorf("setup output:\n%s", h.stdout.String())
	}
	h.stdout.Reset()
	h.run("status")
	if !strings.Contains(h.stdout.String(), "on, 0 note(s)") {
		t.Errorf("status:\n%s", h.stdout.String())
	}
}

func TestMemoryIsOffUnlessAskedFor(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes")
	if _, err := os.Stat(filepath.Join(h.modDir(), ".mcp.json")); err == nil {
		t.Error("the memory server was declared without --memory")
	}
	h.stdout.Reset()
	h.run("status")
	if !strings.Contains(h.stdout.String(), "claude-go setup --memory") {
		t.Errorf("status should say how to turn it on:\n%s", h.stdout.String())
	}
}

func TestAnUpgradedModKeepsTheMemoryDeclaration(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, map[string]string{"a": "a@example.com"})
	h.run("accounts", "add", "a", "--login")
	h.run("setup", "--yes", "--memory")
	os.WriteFile(filepath.Join(h.modDir(), ".claude-plugin", "plugin.json"), []byte(`{"name":"account-flow","version":"0.0.1"}`), 0o644)
	h.run("run", "--account", "a") // syncs the mod: replaces the folder, then must restore the declaration
	if decl := readFile(t, filepath.Join(h.modDir(), ".mcp.json")); !strings.Contains(decl, h.installedBin()) {
		t.Errorf("declaration lost across an upgrade: %s", decl)
	}
}

func TestUndoTurnsMemoryOffButKeepsTheUsersNotes(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes", "--memory")
	h.store(t).Remember(memory.GlobalScope, "keepme", "precious", nil)
	if code := h.run("setup", "--undo"); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(h.home, "memory.enabled")); err == nil {
		t.Error("marker still present")
	}
	if _, err := os.Stat(h.modDir()); err == nil {
		t.Error("mod still installed")
	}
	if facts, _ := h.store(t).Facts(); len(facts) != 1 || facts[0].Key != "keepme" {
		t.Errorf("undo must never delete the user's notes: %+v", facts)
	}
	if !strings.Contains(h.stdout.String(), "your notes are kept") {
		t.Errorf("undo output:\n%s", h.stdout.String())
	}
}

// --- the stdio server through the real command ---

func (h *harness) mcpSession(t *testing.T, lines ...string) (frames []map[string]any, stdout, stderr string) {
	t.Helper()
	env := h.env
	env.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(context.Background(), []string{"mcp-memory"}, env); code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout carried something that is not a protocol frame: %q", sc.Text())
		}
		frames = append(frames, m)
	}
	return frames, out.String(), errb.String()
}

func TestMCPMemoryServesToolsOverStdioAndOnlyFrames(t *testing.T) {
	h := newHarness(t)
	h.gitProject(t, "git@github.com:acme/webapp.git")
	frames, _, stderr := h.mcpSession(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"remember","arguments":{"key":"deploy:target","value":"Fly.io fra","tags":["deploy"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall","arguments":{"topic":"deploy"}}}`,
	)
	if stderr != "" {
		t.Errorf("stderr = %q (a healthy server is silent)", stderr)
	}
	if len(frames) != 4 {
		t.Fatalf("frames = %d: %v", len(frames), frames)
	}
	text := func(f map[string]any) string {
		return f["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !strings.Contains(text(frames[2]), `Saved project webapp note "deploy:target"`) || !strings.Contains(text(frames[3]), "Fly.io fra") {
		t.Errorf("tool results: %q / %q", text(frames[2]), text(frames[3]))
	}
	fi, err := os.Stat(h.memDir())
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("memory dir: %v %v", fi, err)
	}
	if facts, _ := h.store(t).Facts(); len(facts) != 1 || facts[0].Scope != "project:github.com/acme/webapp" {
		t.Errorf("stored = %+v", facts)
	}
}

func TestMCPMemoryWithABrokenConfigFailsWithoutWritingToStdout(t *testing.T) {
	h := newHarness(t)
	h.writeState(t, "config.json", `{"version":1,"memory_dir":"relative/path"}`)
	env := h.env
	env.Stdin = strings.NewReader("")
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr = &out, &errb
	if code := Run(context.Background(), []string{"mcp-memory"}, env); code != exitFailure {
		t.Errorf("code = %d", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty so it cannot corrupt the protocol, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "memory_dir") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestMemoryDirCanBeRelocatedToASyncedFolder(t *testing.T) {
	h := newHarness(t)
	synced := filepath.Join(t.TempDir(), "Dropbox", "claude-go-memory")
	h.writeState(t, "config.json", `{"version":1,"memory_dir":`+jsonString(synced)+`}`)
	h.run("memory", "path")
	if got := strings.TrimSpace(h.stdout.String()); got != synced {
		t.Errorf("path = %q, want %q", got, synced)
	}
	h.gitProject(t, "https://github.com/o/r.git")
	h.mcpSession(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember","arguments":{"key":"k","value":"v","scope":"global"}}}`)
	if _, err := os.Stat(filepath.Join(synced, "machine")); err != nil {
		t.Errorf("notes were not written to the configured folder: %v", err)
	}
	if _, err := os.Stat(h.memDir()); err == nil {
		t.Error("the default folder was used despite memory_dir")
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// --- the management commands ---

func TestMemoryListForgetExportImport(t *testing.T) {
	h := newHarness(t)
	h.gitProject(t, "https://github.com/acme/app.git")
	s := h.store(t)
	s.Now = h.env.Now
	proj, _ := memory.ProjectScope(mustGetwd(t, h))
	s.Remember(proj, "arch", "layered monolith", []string{"arch"})
	s.Remember(memory.GlobalScope, "style", "tabs", nil)
	s.Remember("project:github.com/acme/other", "secretsauce", "another project's note", nil)

	h.run("memory", "list")
	out := h.stdout.String()
	if !strings.Contains(out, "layered monolith") || !strings.Contains(out, "tabs") || strings.Contains(out, "another project") {
		t.Errorf("list (this project + global):\n%s", out)
	}
	h.stdout.Reset()
	h.run("memory", "list", "--all")
	if !strings.Contains(h.stdout.String(), "another project's note") {
		t.Errorf("list --all:\n%s", h.stdout.String())
	}

	bundle := filepath.Join(t.TempDir(), "notes.jsonl")
	if code := h.run("memory", "export", bundle); code != exitOK {
		t.Fatalf("export code=%d %s", code, h.stderr.String())
	}
	if fi, _ := os.Stat(bundle); fi.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %v", fi.Mode().Perm())
	}
	if code := h.run("memory", "export", bundle); code != exitFailure {
		t.Error("export must refuse to overwrite an existing file")
	}

	other := newHarness(t)
	if code := other.run("memory", "import", bundle); code != exitOK || !strings.Contains(other.stdout.String(), "Imported 3 new entries") {
		t.Fatalf("import code=%d out=%s err=%s", code, other.stdout.String(), other.stderr.String())
	}
	other.stdout.Reset()
	other.run("memory", "import", bundle)
	if !strings.Contains(other.stdout.String(), "Imported 0 new entries (3 already known") {
		t.Errorf("re-import: %s", other.stdout.String())
	}

	h.stdout.Reset()
	if code := h.run("memory", "forget", "arch"); code != exitOK || !strings.Contains(h.stdout.String(), "Forgot") {
		t.Errorf("forget code=%d out=%s", code, h.stdout.String())
	}
	if code := h.run("memory", "forget", "arch"); code != exitFailure {
		t.Error("forgetting a missing note should fail")
	}
	if code := h.run("memory", "forget", "--global", "style"); code != exitOK {
		t.Error("forget --global")
	}
}

func mustGetwd(t *testing.T, h *harness) string {
	t.Helper()
	d, err := h.env.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMemoryCommandErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"memory"}, {"memory", "bogus"}, {"memory", "export"}, {"memory", "import"}, {"memory", "forget"}} {
		if code := h.run(args...); code != exitUsage {
			t.Errorf("%v: code = %d", args, code)
		}
	}
	if code := h.run("memory", "import", filepath.Join(t.TempDir(), "missing.jsonl")); code != exitFailure {
		t.Errorf("importing a missing file: code=%d", code)
	}
	h.stdout.Reset()
	h.run("memory", "list")
	if !strings.Contains(h.stdout.String(), "No notes saved yet.") {
		t.Errorf("empty list: %q", h.stdout.String())
	}
}
