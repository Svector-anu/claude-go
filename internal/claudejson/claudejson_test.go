package claudejson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".claude.json")
	if content != "" {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func load(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON written: %v\n%s", err, b)
	}
	return m
}

const source = `{
  "hasCompletedOnboarding": true,
  "lastOnboardingVersion": "2.1.303",
  "hasSeenTasksHint": true,
  "numStartups": 164,
  "oauthAccount": {"emailAddress": "default@example.com"},
  "userID": "default-user-id",
  "machineID": "default-machine",
  "tipsHistory": {"a": 1},
  "projects": {
    "/work/trusted": {"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true, "allowedTools": ["Bash"], "history": ["private prompt"]},
    "/work/untrusted": {"hasTrustDialogAccepted": false},
    "/work/other": {"lastCost": 1.5}
  }
}`

func TestSeedCarriesOnboardingAndTrustAndNothingAboutIdentity(t *testing.T) {
	src, dst := write(t, source), write(t, "")
	changed, err := Seed(src, dst)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got := load(t, dst)
	if got["hasCompletedOnboarding"] != true || got["lastOnboardingVersion"] != "2.1.303" || got["hasSeenTasksHint"] != true {
		t.Errorf("onboarding flags: %v", got)
	}
	for _, forbidden := range []string{"oauthAccount", "userID", "machineID", "tipsHistory", "numStartups"} {
		if _, has := got[forbidden]; has {
			t.Errorf("%s was copied into the profile", forbidden)
		}
	}
	projects := got["projects"].(map[string]any)
	if len(projects) != 1 {
		t.Fatalf("only trusted folders should be carried: %v", projects)
	}
	trusted := projects["/work/trusted"].(map[string]any)
	if trusted["hasTrustDialogAccepted"] != true || trusted["hasCompletedProjectOnboarding"] != true {
		t.Errorf("trust not carried: %v", trusted)
	}
	if _, has := trusted["allowedTools"]; has {
		t.Error("permission rules must not be copied")
	}
	if _, has := trusted["history"]; has {
		t.Error("prompt history must not be copied")
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestSeedNeverOverwritesWhatTheProfileAlreadyDecided(t *testing.T) {
	src := write(t, source)
	dst := write(t, `{"hasCompletedOnboarding": false, "lastOnboardingVersion": "9.9.9", "bigNumber": 12345678901234567890, "oauthAccount": {"emailAddress": "second@example.com"}, "projects": {"/work/trusted": {"hasTrustDialogAccepted": false, "lastCost": 2.5}}}`)
	if _, err := Seed(src, dst); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dst)
	got := load(t, dst)
	if got["hasCompletedOnboarding"] != false || got["lastOnboardingVersion"] != "9.9.9" {
		t.Errorf("an existing decision was overwritten: %v", got)
	}
	if !strings.Contains(string(raw), "12345678901234567890") {
		t.Error("a large number was mangled by a float round trip")
	}
	if got["oauthAccount"].(map[string]any)["emailAddress"] != "second@example.com" {
		t.Error("the profile's own identity was disturbed")
	}
	p := got["projects"].(map[string]any)["/work/trusted"].(map[string]any)
	if p["lastCost"] != 2.5 || p["hasTrustDialogAccepted"] != true {
		t.Errorf("project merge: %v (trust is added, the profile's other fields stay)", p)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	src, dst := write(t, source), write(t, "")
	Seed(src, dst)
	first, _ := os.ReadFile(dst)
	changed, err := Seed(src, dst)
	if err != nil || changed {
		t.Fatalf("second seed changed=%v err=%v", changed, err)
	}
	if second, _ := os.ReadFile(dst); string(first) != string(second) {
		t.Error("an unchanged seed rewrote the file")
	}
}

func TestNewlyTrustedFoldersAreCarriedOnTheNextSeed(t *testing.T) {
	srcPath, dst := write(t, source), write(t, "")
	Seed(srcPath, dst)
	os.WriteFile(srcPath, []byte(`{"projects":{"/work/new":{"hasTrustDialogAccepted":true}}}`), 0o600)
	if changed, _ := Seed(srcPath, dst); !changed {
		t.Fatal("a folder trusted later should be carried")
	}
	projects := load(t, dst)["projects"].(map[string]any)
	if _, has := projects["/work/new"]; !has || len(projects) != 2 {
		t.Errorf("projects = %v", projects)
	}
}

func TestSeedLeavesDamagedOrBusyFilesAlone(t *testing.T) {
	src := write(t, source)
	bad := write(t, `{"half": `)
	if _, err := Seed(src, bad); err == nil {
		t.Error("expected an error for a damaged profile file")
	}
	if b, _ := os.ReadFile(bad); string(b) != `{"half": ` {
		t.Error("a damaged file was modified")
	}
	arr := write(t, `[1,2]`)
	if _, err := Seed(src, arr); err == nil {
		t.Error("a non-object file must be refused")
	}
	badProjects := write(t, `{"projects": 5}`)
	if _, err := Seed(src, badProjects); err == nil {
		t.Error("a malformed projects entry must be refused")
	}
	busy := write(t, `{"x":1}`)
	os.Mkdir(busy+".lock", 0o700) // Claude's own lock while it writes this file
	if changed, err := Seed(src, busy); changed || err != nil {
		t.Errorf("busy file: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(busy); string(b) != `{"x":1}` {
		t.Error("a file Claude holds locked was modified")
	}
}

func TestSeedWithoutASourceDoesNothing(t *testing.T) {
	dst := write(t, "")
	if changed, err := Seed(filepath.Join(t.TempDir(), "none.json"), dst); changed || err != nil {
		t.Errorf("changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a destination was created from nothing")
	}
	// A source with nothing to carry must not create an empty file either.
	if changed, _ := Seed(write(t, `{"oauthAccount":{}}`), dst); changed {
		t.Error("nothing to carry, yet changed")
	}
}

func TestTrustFolderAddsTrustAndKeepsTheRest(t *testing.T) {
	dst := write(t, `{"oauthAccount":{"emailAddress":"second@example.com"},"projects":{"/home/u":{"lastCost":1.5,"hasTrustDialogAccepted":false},"/other":{"hasTrustDialogAccepted":true}}}`)
	changed, err := TrustFolder(dst, "/home/u")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got := load(t, dst)
	projects := got["projects"].(map[string]any)
	home := projects["/home/u"].(map[string]any)
	if home["hasTrustDialogAccepted"] != true || home["lastCost"] != 1.5 {
		t.Errorf("home entry = %v", home)
	}
	if projects["/other"].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Error("another folder's entry was disturbed")
	}
	if got["oauthAccount"].(map[string]any)["emailAddress"] != "second@example.com" {
		t.Error("the profile's own identity was disturbed")
	}
}

func TestTrustFolderCreatesTheEntryAndIsIdempotent(t *testing.T) {
	dst := write(t, `{"hasCompletedOnboarding":true}`)
	if changed, err := TrustFolder(dst, "/home/u"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	first, _ := os.ReadFile(dst)
	if changed, err := TrustFolder(dst, "/home/u"); err != nil || changed {
		t.Fatalf("second call changed=%v err=%v", changed, err)
	}
	if second, _ := os.ReadFile(dst); string(first) != string(second) {
		t.Error("an unchanged call rewrote the file")
	}
	if load(t, dst)["hasCompletedOnboarding"] != true {
		t.Error("an unrelated flag was lost")
	}
}

func TestTrustFolderLeavesDamagedOrBusyFilesAlone(t *testing.T) {
	for name, content := range map[string]string{
		"not json":         `{"half": `,
		"projects not map": `{"projects": 5}`,
		"entry not object": `{"projects":{"/home/u":7}}`,
	} {
		dst := write(t, content)
		if _, err := TrustFolder(dst, "/home/u"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if b, _ := os.ReadFile(dst); string(b) != content {
			t.Errorf("%s: the file was modified", name)
		}
	}
	busy := write(t, `{"x":1}`)
	os.Mkdir(busy+".lock", 0o700)
	if changed, err := TrustFolder(busy, "/home/u"); changed || err != nil {
		t.Errorf("busy file: changed=%v err=%v", changed, err)
	}
}
