package paths

import (
	"errors"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func home(dir string, err error) func() (string, error) {
	return func() (string, error) { return dir, err }
}

func TestResolveDefaultsToHomeDir(t *testing.T) {
	l, err := Resolve(env(nil), home("/Users/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/Users/x", ".claude-go"); l.Root != want {
		t.Errorf("Root = %q, want %q", l.Root, want)
	}
	if l.Config != filepath.Join(l.Root, "config.json") || l.Accounts != filepath.Join(l.Root, "accounts.json") {
		t.Errorf("unexpected layout: %+v", l)
	}
}

func TestResolveEnvOverride(t *testing.T) {
	l, err := Resolve(env(map[string]string{EnvHome: "/tmp/cg"}), home("", errors.New("unused")))
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != "/tmp/cg" {
		t.Errorf("Root = %q", l.Root)
	}
}

func TestResolveRejectsRelativeOverride(t *testing.T) {
	if _, err := Resolve(env(map[string]string{EnvHome: "rel/dir"}), home("/h", nil)); err == nil {
		t.Fatal("expected error for relative override")
	}
}

func TestResolveHomeFailure(t *testing.T) {
	if _, err := Resolve(env(nil), home("", errors.New("boom"))); err == nil {
		t.Fatal("expected error when home is unavailable")
	}
	if _, err := Resolve(env(nil), home("", nil)); err == nil {
		t.Fatal("expected error when home is empty")
	}
}

func TestResolveFillsEveryLocation(t *testing.T) {
	l, err := Resolve(env(nil), home("/Users/x", nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"Config": l.Config, "Accounts": l.Accounts, "Runtime": l.Runtime,
		"State": l.State, "History": l.History, "DefaultClaudeDir": l.DefaultClaudeDir} {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
	}
}
