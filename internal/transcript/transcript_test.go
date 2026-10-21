package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "projects", "-a-b", "sess1.jsonl"), "{}\n")
	put(t, filepath.Join(dir, "projects", "-c", "empty.jsonl"), "")
	if p, ok := Find(dir, "sess1"); !ok || filepath.Base(p) != "sess1.jsonl" {
		t.Errorf("Find = %q, %v", p, ok)
	}
	if _, ok := Find(dir, "empty"); ok {
		t.Error("empty transcript should not count")
	}
	if _, ok := Find(dir, "missing"); ok {
		t.Error("missing transcript found")
	}
	if _, ok := Find("", "sess1"); ok {
		t.Error("empty dir accepted")
	}
	if _, ok := Find(dir, ""); ok {
		t.Error("empty id accepted")
	}
}
