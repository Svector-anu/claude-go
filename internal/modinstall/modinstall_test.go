package modinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func plugin(version string) fstest.MapFS {
	return fstest.MapFS{
		".claude-plugin/plugin.json":      {Data: []byte(`{"name":"account-flow","version":"` + version + `"}`)},
		"hooks/hooks.json":                {Data: []byte(`{"modules":["./register.ts"]}`)},
		"hooks/register.ts":               {Data: []byte("export const register = () => {}")},
		"tests/register.test.ts":          {Data: []byte("test")},
		"hooks/format.test.ts":            {Data: []byte("test")},
		".claude-plugin/types/index.d.ts": {Data: []byte("generated")},
		"skills/claude-go/SKILL.md":       {Data: []byte("skill")},
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestInstallExtractsOnlyWhatShips(t *testing.T) {
	root := t.TempDir()
	v, err := Install(root, plugin("0.1.0"))
	if err != nil || v != "0.1.0" {
		t.Fatalf("v=%q err=%v", v, err)
	}
	d := Dir(root)
	for _, want := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "skills/claude-go/SKILL.md"} {
		if !exists(filepath.Join(d, want)) {
			t.Errorf("missing %s", want)
		}
	}
	for _, not := range []string{"tests", "hooks/format.test.ts", ".claude-plugin/types"} {
		if exists(filepath.Join(d, not)) {
			t.Errorf("%s must not be installed", not)
		}
	}
	if got, ok, _ := Version(root); !ok || got != "0.1.0" {
		t.Errorf("Version = %q %v", got, ok)
	}
}

func TestInstallReplacesThePreviousVersionCompletely(t *testing.T) {
	root := t.TempDir()
	Install(root, plugin("0.1.0"))
	stale := filepath.Join(Dir(root), "hooks", "removed-in-new-version.ts")
	os.WriteFile(stale, []byte("old"), 0o644)
	if v, err := Install(root, plugin("0.2.0")); err != nil || v != "0.2.0" {
		t.Fatalf("v=%q err=%v", v, err)
	}
	if exists(stale) {
		t.Error("a file from the old version survived the upgrade")
	}
	entries, _ := os.ReadDir(filepath.Dir(Dir(root)))
	if len(entries) != 1 {
		t.Errorf("staging leftovers: %v", entries)
	}
}

func TestInstallRefusesAMalformedPlugin(t *testing.T) {
	root := t.TempDir()
	Install(root, plugin("0.1.0"))
	bad := fstest.MapFS{".claude-plugin/plugin.json": {Data: []byte(`{"name":"other"}`)}}
	if _, err := Install(root, bad); err == nil {
		t.Fatal("expected error")
	}
	if v, ok, _ := Version(root); !ok || v != "0.1.0" {
		t.Errorf("a failed install damaged the working one: %q %v", v, ok)
	}
}

func TestRemoveOnlyRemovesTheMod(t *testing.T) {
	root := t.TempDir()
	if removed, err := Remove(root); removed || err != nil {
		t.Errorf("removing nothing: %v %v", removed, err)
	}
	Install(root, plugin("0.1.0"))
	if removed, err := Remove(root); !removed || err != nil {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if exists(Dir(root)) || exists(filepath.Dir(Dir(root))) {
		t.Error("mod folder left behind")
	}
	// A folder at that path that is not the mod is never deleted.
	os.MkdirAll(filepath.Join(Dir(root), ".claude-plugin"), 0o755)
	os.WriteFile(filepath.Join(Dir(root), ".claude-plugin", "plugin.json"), []byte(`{"name":"somebody-else"}`), 0o644)
	os.WriteFile(filepath.Join(Dir(root), "precious"), []byte("x"), 0o644)
	if removed, err := Remove(root); removed || err == nil {
		t.Errorf("removed=%v err=%v", removed, err)
	}
	if !exists(filepath.Join(Dir(root), "precious")) {
		t.Error("someone else's folder was deleted")
	}
}

func TestMergePluginDirs(t *testing.T) {
	sep := string(os.PathListSeparator)
	cases := []struct{ existing, dir, want string }{
		{"", "/m", "/m"},
		{"/a", "/m", "/a" + sep + "/m"},
		{"/a" + sep + "/m", "/m", "/a" + sep + "/m"},
		{"/m/", "/m", "/m/"},
	}
	for _, c := range cases {
		if got := MergePluginDirs(c.existing, c.dir); got != c.want {
			t.Errorf("Merge(%q,%q) = %q, want %q", c.existing, c.dir, got, c.want)
		}
	}
}

func TestWriteMCPPointsAtTheBinaryAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteMCP(root, "/bin/claude-go"); err == nil {
		t.Fatal("writing an MCP declaration without the mod must fail")
	}
	Install(root, plugin("0.1.0"))
	if changed, err := WriteMCP(root, "/home/u/.local/bin/claude-go"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if changed, _ := WriteMCP(root, "/home/u/.local/bin/claude-go"); changed {
		t.Error("an identical declaration must not be rewritten")
	}
	if b, ok := MCPBinary(root); !ok || b != "/home/u/.local/bin/claude-go" {
		t.Errorf("MCPBinary = %q %v", b, ok)
	}
	data, _ := os.ReadFile(filepath.Join(Dir(root), MCPFile))
	for _, want := range []string{`"mcpServers"`, `"memory"`, `"mcp-memory"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %s in %s", want, data)
		}
	}
	if changed, _ := WriteMCP(root, "/other/claude-go"); !changed {
		t.Error("a new binary path must update the file")
	}
}

func TestReinstallingTheModDropsTheGeneratedDeclaration(t *testing.T) {
	root := t.TempDir()
	Install(root, plugin("0.1.0"))
	WriteMCP(root, "/bin/claude-go")
	Install(root, plugin("0.2.0"))
	if _, ok := MCPBinary(root); ok {
		t.Error("the generated file must be regenerated by the caller after an upgrade, not silently kept stale")
	}
}
