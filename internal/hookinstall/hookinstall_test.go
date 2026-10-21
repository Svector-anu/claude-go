package hookinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bin = "/home/u/.local/bin/claude-go"

func newInstaller(t *testing.T, content string) (Installer, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Installer{SettingsPath: p, Binary: bin}, p
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("invalid JSON written: %v\n%s", err, data)
	}
	return m
}

func commandsFor(m map[string]any, event string) []string {
	var out []string
	hooks, _ := m["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func TestIsOurs(t *testing.T) {
	yes := []string{
		"claude-go hook stop-failure",
		"/home/u/.local/bin/claude-go hook stop-failure",
		"'/Users/some one/bin/claude-go' hook stop-failure",
		`"/x/claude-go" hook anything`,
	}
	no := []string{
		"", "claude-go", "claude-go run", "cux hook stop", "/bin/claude-go-other hook x",
		"echo claude-go hook", "my-claude-go hook stop-failure",
	}
	for _, c := range yes {
		if !IsOurs(c) {
			t.Errorf("should be ours: %q", c)
		}
	}
	for _, c := range no {
		if IsOurs(c) {
			t.Errorf("must not claim: %q", c)
		}
	}
}

func TestCommandQuotesAwkwardPaths(t *testing.T) {
	c := Command("/Users/some one/it's/claude-go")
	if !IsOurs(c) {
		t.Errorf("round trip failed for %q", c)
	}
	if f := fields(c); f[0] != "/Users/some one/it's/claude-go" {
		t.Errorf("fields = %q", f)
	}
}

func TestInstallIntoMissingFile(t *testing.T) {
	in, p := newInstaller(t, "")
	res, err := in.Install()
	if err != nil || !res.Changed || res.Backup != "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := commandsFor(readJSON(t, p), Event); len(got) != 1 || got[0] != Command(bin) {
		t.Errorf("commands = %v", got)
	}
	if st, _ := in.Status(); st != Installed {
		t.Errorf("status = %v", st)
	}
}

func TestInstallPreservesEverythingElse(t *testing.T) {
	orig := `{
  "model": "opus",
  "env": {"A": "1"},
  "permissions": {"allow": ["Bash(ls)"]},
  "bigNumber": 12345678901234567890,
  "hooks": {
    "StopFailure": [{"matcher": "rate_limit", "hooks": [{"type": "command", "command": "other-tool notify", "timeout": 5}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]
  }
}`
	in, p := newInstaller(t, orig)
	res, err := in.Install()
	if err != nil || !res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	m := readJSON(t, p)
	if m["model"] != "opus" {
		t.Error("model lost")
	}
	if got := commandsFor(m, "PreToolUse"); len(got) != 1 || got[0] != "guard.sh" {
		t.Errorf("unrelated event damaged: %v", got)
	}
	got := commandsFor(m, Event)
	if len(got) != 2 || got[0] != "other-tool notify" || got[1] != Command(bin) {
		t.Errorf("StopFailure commands = %v", got)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "12345678901234567890") {
		t.Error("large number was mangled by a float round trip")
	}
	b, err := os.ReadFile(p + backupSuffix)
	if err != nil || string(b) != orig {
		t.Errorf("backup missing or different: %v", err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode changed to %v", fi.Mode().Perm())
	}
}

func TestInstallIsIdempotentAndBackupIsWrittenOnce(t *testing.T) {
	in, p := newInstaller(t, `{"model":"opus"}`)
	in.Install()
	first, _ := os.ReadFile(p)
	res, err := in.Install()
	if err != nil || res.Changed {
		t.Fatalf("second install: %+v %v", res, err)
	}
	second, _ := os.ReadFile(p)
	if string(first) != string(second) {
		t.Error("idempotent install rewrote the file")
	}
	if b, _ := os.ReadFile(p + backupSuffix); string(b) != `{"model":"opus"}` {
		t.Errorf("backup was overwritten: %q", b)
	}
}

func TestInstallUpdatesStalePathAndDedupes(t *testing.T) {
	old := `{"hooks":{"StopFailure":[
	  {"hooks":[{"type":"command","command":"/old/place/claude-go hook stop-failure","timeout":10}]},
	  {"hooks":[{"type":"command","command":"claude-go hook stop-failure"},{"type":"command","command":"keep-me"}]}
	]}}`
	in, p := newInstaller(t, old)
	if st, _ := in.Status(); st != Stale {
		t.Errorf("status = %v, want Stale", st)
	}
	if _, err := in.Install(); err != nil {
		t.Fatal(err)
	}
	got := commandsFor(readJSON(t, p), Event)
	if len(got) != 2 || got[0] != Command(bin) || got[1] != "keep-me" {
		t.Errorf("commands = %v", got)
	}
	if st, _ := in.Status(); st != Installed {
		t.Errorf("status after = %v", st)
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	in, p := newInstaller(t, `{"model":"opus","hooks":{"StopFailure":[{"hooks":[{"type":"command","command":"keep"}]}]}}`)
	in.Install()
	res, err := in.Uninstall()
	if err != nil || !res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	m := readJSON(t, p)
	if got := commandsFor(m, Event); len(got) != 1 || got[0] != "keep" {
		t.Errorf("commands = %v", got)
	}
	if res, _ := in.Uninstall(); res.Changed {
		t.Error("second uninstall should be a no-op")
	}
}

func TestUninstallCleansEmptyContainers(t *testing.T) {
	in, p := newInstaller(t, `{"model":"opus"}`)
	in.Install()
	in.Uninstall()
	m := readJSON(t, p)
	if _, ok := m["hooks"]; ok || m["model"] != "opus" {
		t.Errorf("settings = %v", m)
	}
}

func TestInvalidSettingsAreNeverModified(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       `{"model": `,
		"array":          `[1,2]`,
		"hooks is array": `{"hooks": []}`,
	} {
		in, p := newInstaller(t, content)
		if _, err := in.Install(); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if got, _ := os.ReadFile(p); string(got) != content {
			t.Errorf("%s: file modified", name)
		}
		if _, err := os.Stat(p + backupSuffix); err == nil {
			t.Errorf("%s: backup written for a file we did not change", name)
		}
	}
}

func TestSymlinkedSettingsAreWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte(`{"model":"opus"}`), 0o600)
	link := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(link), 0o700)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	in := Installer{SettingsPath: link, Binary: bin}
	if _, err := in.Install(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
	if got := commandsFor(readJSON(t, real), Event); len(got) != 1 {
		t.Errorf("target not updated: %v", got)
	}
}

func TestEmptyFileIsTreatedAsEmptySettings(t *testing.T) {
	in, p := newInstaller(t, "  \n")
	if _, err := in.Install(); err != nil {
		t.Fatal(err)
	}
	if got := commandsFor(readJSON(t, p), Event); len(got) != 1 {
		t.Errorf("commands = %v", got)
	}
}

func TestInstalledBinary(t *testing.T) {
	in, _ := newInstaller(t, "")
	if _, found, err := in.InstalledBinary(); found || err != nil {
		t.Errorf("found=%v err=%v", found, err)
	}
	in.Install()
	if b, found, err := in.InstalledBinary(); !found || err != nil || b != bin {
		t.Errorf("b=%q found=%v err=%v", b, found, err)
	}
}

func statusLineOf(t *testing.T, p string) map[string]any {
	t.Helper()
	m, _ := readJSON(t, p)["statusLine"].(map[string]any)
	return m
}

func TestStatusLineAddedWhenAbsent(t *testing.T) {
	in, p := newInstaller(t, `{"model":"opus"}`)
	res, mode, err := in.InstallStatusLine()
	if err != nil || mode != StatusLineAdded || !res.Changed {
		t.Fatalf("res=%+v mode=%s err=%v", res, mode, err)
	}
	sl := statusLineOf(t, p)
	if sl["type"] != "command" || sl["command"] != bin+" statusline" {
		t.Errorf("statusLine = %v", sl)
	}
	if readJSON(t, p)["model"] != "opus" {
		t.Error("unrelated settings lost")
	}
	if on, _ := in.StatusLineInstalled(); !on {
		t.Error("StatusLineInstalled = false")
	}
}

func TestStatusLineWrapsExistingCommandAndKeepsItsOtherKeys(t *testing.T) {
	orig := `jq -r '"[\(.model.display_name)] \(.context_window.used_percentage // 0)%"'`
	in, p := newInstaller(t, `{"statusLine":{"type":"command","command":`+jsonString(orig)+`,"padding":2,"refreshInterval":5}}`)
	_, mode, err := in.InstallStatusLine()
	if err != nil || mode != StatusLineWrapped {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
	sl := statusLineOf(t, p)
	cmd := sl["command"].(string)
	if !IsOurStatusLine(cmd) {
		t.Fatalf("command = %q", cmd)
	}
	if got, ok := WrappedStatusLine(cmd); !ok || got != orig {
		t.Errorf("wrapped original = %q (ok=%v), want %q", got, ok, orig)
	}
	if sl["padding"] == nil || sl["refreshInterval"] == nil {
		t.Errorf("sibling keys lost: %v", sl)
	}
	// Idempotent.
	if res, mode, _ := in.InstallStatusLine(); mode != StatusLineUnchanged || res.Changed {
		t.Errorf("second install: mode=%s changed=%v", mode, res.Changed)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestStatusLineUninstallRestoresExactlyWhatWasThere(t *testing.T) {
	orig := `echo "it's $HOME" | tr a-z A-Z`
	in, p := newInstaller(t, `{"statusLine":{"type":"command","command":`+jsonString(orig)+`}}`)
	in.InstallStatusLine()
	if res, err := in.UninstallStatusLine(); err != nil || !res.Changed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := statusLineOf(t, p)["command"]; got != orig {
		t.Errorf("restored %q, want %q", got, orig)
	}
}

func TestStatusLineUninstallRemovesOnlyWhatWeAdded(t *testing.T) {
	in, p := newInstaller(t, `{"model":"opus"}`)
	in.InstallStatusLine()
	in.UninstallStatusLine()
	m := readJSON(t, p)
	if _, ok := m["statusLine"]; ok || m["model"] != "opus" {
		t.Errorf("settings = %v", m)
	}
	// Someone else's status line is never touched by uninstall.
	in2, p2 := newInstaller(t, `{"statusLine":{"type":"command","command":"theirs.sh"}}`)
	before, _ := os.ReadFile(p2)
	if res, err := in2.UninstallStatusLine(); err != nil || res.Changed {
		t.Errorf("res=%+v err=%v", res, err)
	}
	if after, _ := os.ReadFile(p2); string(after) != string(before) {
		t.Error("foreign status line was modified")
	}
}

func TestStatusLineUpdatesStaleBinaryKeepingTheWrappedCommand(t *testing.T) {
	in, p := newInstaller(t, `{"statusLine":{"type":"command","command":`+jsonString(StatusLineCommand("/old/claude-go", "theirs.sh"))+`}}`)
	_, mode, err := in.InstallStatusLine()
	if err != nil || mode != StatusLineUpdated {
		t.Fatalf("mode=%s err=%v", mode, err)
	}
	cmd := statusLineOf(t, p)["command"].(string)
	if orig, _ := WrappedStatusLine(cmd); orig != "theirs.sh" || !strings.HasPrefix(cmd, bin) {
		t.Errorf("command = %q", cmd)
	}
}

func TestStatusLineOfUnexpectedShapeIsNeverModified(t *testing.T) {
	for name, content := range map[string]string{
		"string":      `{"statusLine":"echo hi"}`,
		"not command": `{"statusLine":{"type":"static","text":"x"}}`,
		"no command":  `{"statusLine":{"type":"command"}}`,
	} {
		in, p := newInstaller(t, content)
		if _, _, err := in.InstallStatusLine(); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if got, _ := os.ReadFile(p); string(got) != content {
			t.Errorf("%s: file modified", name)
		}
	}
}
