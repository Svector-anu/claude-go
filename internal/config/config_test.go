package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingReturnsDefaults(t *testing.T) {
	cfg, exists, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if cfg.Version != currentVersion || cfg.ClaudePath != "" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadValid(t *testing.T) {
	cfg, exists, err := Load(write(t, `{"version":1,"claude_path":"/opt/claude"}`))
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if cfg.ClaudePath != "/opt/claude" {
		t.Errorf("ClaudePath = %q", cfg.ClaudePath)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"corrupt":         `{"version":1,`,
		"unknown field":   `{"version":1,"claude_pth":"x"}`,
		"future version":  `{"version":2}`,
		"missing version": `{}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := write(t, content)
			_, exists, err := Load(p)
			if err == nil {
				t.Fatal("expected error")
			}
			if !exists {
				t.Error("exists should be true for an unreadable-but-present file")
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error should name the file: %v", err)
			}
		})
	}
}

func TestContinuePrompt(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"default is on", Config{}, DefaultContinueMessage},
		{"explicitly on", Config{ContinueAfterLimit: &on}, DefaultContinueMessage},
		{"off", Config{ContinueAfterLimit: &off}, ""},
		{"off wins over a message", Config{ContinueAfterLimit: &off, ContinueMessage: "go"}, ""},
		{"custom message", Config{ContinueMessage: "  Please resume.  "}, "Please resume."},
		{"blank custom message falls back", Config{ContinueMessage: "   "}, DefaultContinueMessage},
	}
	for _, c := range cases {
		if got := c.cfg.ContinuePrompt(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestContinueSettingsLoad(t *testing.T) {
	cfg, _, err := Load(write(t, `{"version":1,"continue_after_limit":false,"continue_message":"x"}`))
	if err != nil || cfg.ContinuePrompt() != "" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, _, err := Load(write(t, `{"version":1,"continue_after_limit":"no"}`)); err == nil {
		t.Error("a non-boolean setting must be rejected")
	}
}
