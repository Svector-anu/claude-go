package shellrc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".zshrc")
	if content != "" {
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestRCFile(t *testing.T) {
	home := t.TempDir()
	if p, ok := RCFile("/bin/zsh", home); !ok || p != filepath.Join(home, ".zshrc") {
		t.Errorf("zsh: %q %v", p, ok)
	}
	if p, _ := RCFile("/bin/bash", home); p != filepath.Join(home, ".bashrc") {
		t.Errorf("bash default: %q", p)
	}
	os.WriteFile(filepath.Join(home, ".bash_profile"), nil, 0o644)
	if p, _ := RCFile("/bin/bash", home); p != filepath.Join(home, ".bash_profile") {
		t.Errorf("bash with .bash_profile: %q", p)
	}
	if _, ok := RCFile("/usr/bin/fish", home); ok {
		t.Error("fish should be unsupported")
	}
}

func TestApplyAppendsAndPreservesExistingContent(t *testing.T) {
	p := tmp(t, "export A=1\nalias ll='ls -l'")
	changed, err := Apply(p, []string{"alias claude='x run'"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(got), "export A=1\nalias ll='ls -l'\n# >>> claude-go >>>\nalias claude='x run'\n# <<< claude-go <<<\n") {
		t.Errorf("content:\n%s", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(p + backupSuffix); string(b) != "export A=1\nalias ll='ls -l'" {
		t.Errorf("backup = %q", b)
	}
}

func TestApplyIsIdempotentAndReplacesBlock(t *testing.T) {
	p := tmp(t, "before\n")
	Apply(p, []string{"one"})
	if changed, _ := Apply(p, []string{"one"}); changed {
		t.Error("second apply should be a no-op")
	}
	if changed, _ := Apply(p, []string{"two"}); !changed {
		t.Error("new content should change the file")
	}
	got, _ := os.ReadFile(p)
	if strings.Count(string(got), "# >>> claude-go >>>") != 1 || !strings.Contains(string(got), "two") || strings.Contains(string(got), "one") {
		t.Errorf("content:\n%s", got)
	}
}

func TestApplyCreatesMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", ".zshrc")
	if changed, err := Apply(p, []string{"x"}); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestRemove(t *testing.T) {
	p := tmp(t, "keep1\n")
	Apply(p, []string{"x"})
	os.WriteFile(p, append(mustRead(t, p), []byte("keep2\n")...), 0o644)
	changed, err := Remove(p)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := mustRead(t, p); string(got) != "keep1\nkeep2\n" {
		t.Errorf("content = %q", got)
	}
	if changed, _ := Remove(p); changed {
		t.Error("second remove should be a no-op")
	}
	if changed, err := Remove(filepath.Join(t.TempDir(), "none")); changed || err != nil {
		t.Errorf("missing file: %v %v", changed, err)
	}
}

func TestDamagedBlockIsNeverGuessedAt(t *testing.T) {
	content := "a\n# >>> claude-go >>>\nalias claude=x\n"
	p := tmp(t, content)
	if _, err := Apply(p, []string{"y"}); err == nil {
		t.Error("Apply must refuse a block without an end marker")
	}
	if _, err := Remove(p); err == nil {
		t.Error("Remove must refuse a block without an end marker")
	}
	if got := mustRead(t, p); string(got) != content {
		t.Error("damaged file was modified")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
