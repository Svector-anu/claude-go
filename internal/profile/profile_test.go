package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T, id string) (Manager, string) {
	t.Helper()
	m := Manager{Root: t.TempDir()}
	dir, _, err := m.Ensure(id)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestLinkSharedProjectsCreatesLinkAndIsIdempotent(t *testing.T) {
	m, dir := setup(t, "a")
	for i := 0; i < 2; i++ {
		if err := m.LinkSharedProjects("a"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.Readlink(filepath.Join(dir, "projects"))
	if err != nil || got != m.SharedProjects() {
		t.Errorf("link = %q, %v", got, err)
	}
}

func TestSharedProjectsVisibleAcrossProfiles(t *testing.T) {
	m, _ := setup(t, "a")
	if _, _, err := m.Ensure("b"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := m.LinkSharedProjects(id); err != nil {
			t.Fatal(err)
		}
	}
	pa, _ := m.Path("a")
	pb, _ := m.Path("b")
	if err := os.MkdirAll(filepath.Join(pa, "projects", "-tmp-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pa, "projects", "-tmp-x", "s.jsonl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pb, "projects", "-tmp-x", "s.jsonl")); err != nil {
		t.Errorf("profile b cannot see profile a's transcript: %v", err)
	}
}

func TestLinkSharedProjectsReplacesEmptyDir(t *testing.T) {
	m, dir := setup(t, "a")
	if err := os.Mkdir(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkSharedProjects("a"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, "projects")); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("empty dir not replaced by link")
	}
}

func TestLinkSharedProjectsRefusesExistingHistory(t *testing.T) {
	m, dir := setup(t, "a")
	hist := filepath.Join(dir, "projects", "-p")
	if err := os.MkdirAll(hist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkSharedProjects("a"); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(hist); err != nil {
		t.Error("existing history was touched")
	}
}

func TestLinkSharedProjectsRefusesForeignLink(t *testing.T) {
	m, dir := setup(t, "a")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkSharedProjects("a"); err == nil {
		t.Fatal("expected refusal for link to another location")
	}
}

func TestSharedProjectsUsesDefaultClaudeDir(t *testing.T) {
	def := filepath.Join(t.TempDir(), ".claude")
	m := Manager{Root: t.TempDir(), DefaultDir: def}
	if _, _, err := m.Ensure("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.LinkSharedProjects("a"); err != nil {
		t.Fatal(err)
	}
	pa, _ := m.Path("a")
	if got, _ := os.Readlink(filepath.Join(pa, "projects")); got != filepath.Join(def, "projects") {
		t.Errorf("link = %q", got)
	}
}

func TestLinkUserConfig(t *testing.T) {
	def := t.TempDir()
	for _, f := range []string{"settings.json", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(def, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(def, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := Manager{Root: t.TempDir(), DefaultDir: def}
	dir, _, _ := m.Ensure("a")
	own := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(own, []byte("profile's own"), 0o600); err != nil {
		t.Fatal(err)
	}

	linked, skipped, err := m.LinkUserConfig("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) != 2 || len(skipped) != 1 || skipped[0] != "CLAUDE.md" {
		t.Errorf("linked=%v skipped=%v", linked, skipped)
	}
	if got, _ := os.ReadFile(own); string(got) != "profile's own" {
		t.Error("existing profile file was overwritten")
	}
	if tgt, _ := os.Readlink(filepath.Join(dir, "settings.json")); tgt != filepath.Join(def, "settings.json") {
		t.Errorf("settings link = %q", tgt)
	}
	if _, err := os.Lstat(filepath.Join(dir, "keybindings.json")); err == nil {
		t.Error("nonexistent source should not be linked")
	}
	linked, _, err = m.LinkUserConfig("a")
	if err != nil || len(linked) != 0 {
		t.Errorf("not idempotent: %v %v", linked, err)
	}
}

func TestLinkUserConfigNeedsDefaultDir(t *testing.T) {
	m := Manager{Root: t.TempDir()}
	m.Ensure("a")
	if _, _, err := m.LinkUserConfig("a"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProvisionCarriesOnboardingAndTrustIntoANewProfile(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	defJSON := filepath.Join(home, ".claude.json")
	os.WriteFile(defJSON, []byte(`{"hasCompletedOnboarding":true,"oauthAccount":{"emailAddress":"default@example.com"},"projects":{"/w":{"hasTrustDialogAccepted":true}}}`), 0o600)
	m := Manager{Root: root, DefaultDir: filepath.Join(home, ".claude"), DefaultClaudeJSON: defJSON}
	dir, _, warnings, err := m.Provision("b")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(b), `"hasCompletedOnboarding": true`) || !strings.Contains(string(b), "hasTrustDialogAccepted") {
		t.Errorf("profile state = %s", b)
	}
	if strings.Contains(string(b), "default@example.com") {
		t.Error("the default account's identity leaked into the profile")
	}
}

func TestADamagedProfileStateFileIsAWarningNotAFailure(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	defJSON := filepath.Join(home, ".claude.json")
	os.WriteFile(defJSON, []byte(`{"hasCompletedOnboarding":true}`), 0o600)
	m := Manager{Root: root, DefaultClaudeJSON: defJSON}
	dir, _, _, _ := m.Provision("b")
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{broken`), 0o600)
	_, _, warnings, err := m.Provision("b")
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "onboarding") {
		t.Errorf("err=%v warnings=%v", err, warnings)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".claude.json")); string(b) != `{broken` {
		t.Error("a damaged file was overwritten")
	}
}

func TestProvisionTrustsTheHomeFolderThatHoldsTheDefaultConfigDir(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	defJSON := filepath.Join(home, ".claude.json")
	os.WriteFile(defJSON, []byte(`{"hasCompletedOnboarding":true}`), 0o600)
	m := Manager{Root: root, DefaultDir: filepath.Join(home, ".claude"), DefaultClaudeJSON: defJSON}
	dir, _, warnings, err := m.Provision("b")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(b), filepath.ToSlash(home)) || !strings.Contains(string(b), `"hasTrustDialogAccepted": true`) {
		t.Errorf("the home folder was not trusted in the profile: %s", b)
	}
}

func TestProvisionTrustsNothingWhenTheDefaultDirIsNotDotClaude(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	defJSON := filepath.Join(home, ".claude.json")
	os.WriteFile(defJSON, []byte(`{"hasCompletedOnboarding":true}`), 0o600)
	m := Manager{Root: root, DefaultDir: filepath.Join(home, "custom-config"), DefaultClaudeJSON: defJSON}
	dir, _, _, err := m.Provision("b")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".claude.json")); strings.Contains(string(b), "hasTrustDialogAccepted") {
		t.Errorf("a folder was trusted without cause: %s", b)
	}
}
