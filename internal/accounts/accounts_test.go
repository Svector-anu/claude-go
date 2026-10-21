package accounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/paths"
	"github.com/Svector-anu/claude-go/internal/registry"
)

func newService(t *testing.T) Service {
	t.Helper()
	root := t.TempDir()
	return Service{
		Layout: paths.Layout{Root: root, Accounts: filepath.Join(root, "accounts.json")},
		Now:    func() time.Time { return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) },
	}
}

func TestAddCreatesRegistryEntryAndPrivateProfile(t *testing.T) {
	s := newService(t)
	res, err := s.Add(registry.Account{ID: "work", Email: "w@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReusedProfile {
		t.Error("fresh profile reported as reused")
	}
	fi, err := os.Stat(res.ProfileDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("profile dir: %v %v", fi, err)
	}
	f, _ := registry.Load(s.Layout.Accounts)
	if len(f.Accounts) != 1 || f.Accounts[0].Label != "work" || !f.Accounts[0].CreatedAt.Equal(s.Now()) {
		t.Errorf("registry: %+v", f)
	}
}

func TestAddDuplicateIsRejectedAndLeavesStateIntact(t *testing.T) {
	s := newService(t)
	if _, err := s.Add(registry.Account{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.Layout.Root, "profiles", "a", "keep")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(registry.Account{ID: "a"}); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("existing profile content disturbed")
	}
}

func TestAddRejectsBadID(t *testing.T) {
	s := newService(t)
	if _, err := s.Add(registry.Account{ID: "../x"}); !errors.Is(err, registry.ErrInvalidID) {
		t.Fatalf("err = %v", err)
	}
}

func TestAddAdoptsExistingProfileWithoutTouchingIt(t *testing.T) {
	s := newService(t)
	dir := filepath.Join(s.Layout.Root, "profiles", "old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "login-state")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := s.Add(registry.Account{ID: "old"})
	if err != nil || !res.ReusedProfile {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("adopted profile content lost")
	}
}

func TestAddFailureRollsBackNewProfile(t *testing.T) {
	s := newService(t)
	if err := os.WriteFile(s.Layout.Accounts, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(registry.Account{ID: "a"}); err == nil {
		t.Fatal("expected corrupt-registry error")
	}
	if _, err := os.Stat(filepath.Join(s.Layout.Root, "profiles", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("profile dir created despite failure: %v", err)
	}
	if got, _ := os.ReadFile(s.Layout.Accounts); string(got) != "garbage" {
		t.Error("corrupt registry was overwritten")
	}
}

func TestProfilesAreIsolated(t *testing.T) {
	s := newService(t)
	a, _ := s.Add(registry.Account{ID: "a"})
	b, _ := s.Add(registry.Account{ID: "b"})
	if a.ProfileDir == b.ProfileDir || filepath.Dir(a.ProfileDir) != filepath.Dir(b.ProfileDir) {
		t.Errorf("profiles not distinct siblings: %q %q", a.ProfileDir, b.ProfileDir)
	}
	if err := os.WriteFile(filepath.Join(a.ProfileDir, "secret"), []byte("a-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.ProfileDir, "secret")); err == nil {
		t.Error("profile b can see profile a's file")
	}
}

func TestRegistryStoresNoCredentialFields(t *testing.T) {
	s := newService(t)
	if _, err := s.Add(registry.Account{ID: "a", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.Layout.Accounts)
	for _, banned := range []string{"token", "secret", "password", "refresh"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("registry contains %q", banned)
		}
	}
}

func TestRemoveKeepsProfileByDefault(t *testing.T) {
	s := newService(t)
	add, _ := s.Add(registry.Account{ID: "a"})
	res, err := s.Remove("a", false)
	if err != nil || res.Purged {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(add.ProfileDir); err != nil {
		t.Error("profile removed without purge")
	}
	if f, _ := registry.Load(s.Layout.Accounts); len(f.Accounts) != 0 {
		t.Errorf("still registered: %+v", f)
	}
}

func TestRemovePurge(t *testing.T) {
	s := newService(t)
	add, _ := s.Add(registry.Account{ID: "a"})
	if err := os.WriteFile(filepath.Join(add.ProfileDir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := s.Remove("a", true)
	if err != nil || !res.Purged {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(add.ProfileDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("profile still present: %v", err)
	}
}

func TestRemovePurgeRefusesSymlinkedProfile(t *testing.T) {
	s := newService(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(registry.Account{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Layout.Root, "profiles", "a")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove("a", true); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("data behind symlink was deleted")
	}
}

func TestRemoveUnknown(t *testing.T) {
	s := newService(t)
	if _, err := s.Remove("ghost", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentAddsAllLand(t *testing.T) {
	s := newService(t)
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Add(registry.Account{ID: fmt.Sprintf("acct-%d", i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	f, err := registry.Load(s.Layout.Accounts)
	if err != nil || len(f.Accounts) != n {
		t.Fatalf("accounts=%d err=%v", len(f.Accounts), err)
	}
}

func loginAs(email, org string) LoginFunc {
	return func(dir string) (registry.Account, error) {
		// Stand-in for a real login: the profile gains a marker, as Claude would add credentials.
		if err := os.WriteFile(filepath.Join(dir, "logged-in"), nil, 0o600); err != nil {
			return registry.Account{}, err
		}
		return registry.Account{Email: email, OrgID: org}, nil
	}
}

func TestAddWithLoginRecordsIdentity(t *testing.T) {
	s := newService(t)
	res, err := s.AddWithLogin(registry.Account{ID: "b"}, loginAs("b@example.com", "org-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.ProfileDir, "logged-in")); err != nil {
		t.Error("login ran in the wrong directory")
	}
	f, _ := registry.Load(s.Layout.Accounts)
	if f.Accounts[0].Email != "b@example.com" || f.Accounts[0].OrgID != "org-b" {
		t.Errorf("identity not recorded: %+v", f.Accounts[0])
	}
}

func TestAddWithLoginFailureRegistersNothingAndRemovesNewProfile(t *testing.T) {
	s := newService(t)
	_, err := s.AddWithLogin(registry.Account{ID: "b"}, func(string) (registry.Account, error) {
		return registry.Account{}, errors.New("browser closed")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if f, _ := registry.Load(s.Layout.Accounts); len(f.Accounts) != 0 {
		t.Errorf("registered despite failed login: %+v", f.Accounts)
	}
	if _, err := os.Stat(filepath.Join(s.Layout.Root, "profiles", "b")); !errors.Is(err, os.ErrNotExist) {
		t.Error("profile created by the failed call was left behind")
	}
}

func TestAddWithLoginFailureKeepsPreexistingProfile(t *testing.T) {
	s := newService(t)
	dir := filepath.Join(s.Layout.Root, "profiles", "b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _ = s.AddWithLogin(registry.Account{ID: "b"}, func(string) (registry.Account, error) {
		return registry.Account{}, errors.New("fail")
	})
	if _, err := os.Stat(dir); err != nil {
		t.Error("a profile that existed before the call must never be deleted")
	}
}

func TestAddWithLoginRejectsSameLoginAsExistingAccount(t *testing.T) {
	s := newService(t)
	if _, err := s.AddWithLogin(registry.Account{ID: "a"}, loginAs("x@example.com", "org")); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddWithLogin(registry.Account{ID: "b"}, loginAs("x@example.com", "org"))
	if !errors.Is(err, ErrSameLogin) {
		t.Fatalf("err = %v", err)
	}
	if f, _ := registry.Load(s.Layout.Accounts); len(f.Accounts) != 1 {
		t.Errorf("twin registered: %+v", f.Accounts)
	}
	if _, err := os.Stat(filepath.Join(s.Layout.Root, "profiles", "b")); !errors.Is(err, os.ErrNotExist) {
		t.Error("twin profile left behind")
	}
}

func TestSameEmailDifferentOrgIsADifferentLogin(t *testing.T) {
	s := newService(t)
	s.AddWithLogin(registry.Account{ID: "a"}, loginAs("x@example.com", "org1"))
	if _, err := s.AddWithLogin(registry.Account{ID: "b"}, loginAs("x@example.com", "org2")); err != nil {
		t.Fatalf("personal and team logins of one email are distinct: %v", err)
	}
}

func TestAdoptDefaultIsIdempotentAndFirst(t *testing.T) {
	s := newService(t)
	s.Add(registry.Account{ID: "extra"})
	cfg := filepath.Join(t.TempDir(), ".claude")
	a, adopted, err := s.AdoptDefault("default", registry.Account{Email: "me@example.com", OrgID: "o"}, cfg)
	if err != nil || !adopted || !a.Default || a.ConfigDir != cfg {
		t.Fatalf("a=%+v adopted=%v err=%v", a, adopted, err)
	}
	if again, adopted, err := s.AdoptDefault("default", registry.Account{Email: "me@example.com"}, cfg); err != nil || adopted || again.ID != "default" {
		t.Errorf("second adopt: %+v %v %v", again, adopted, err)
	}
	f, _ := registry.Load(s.Layout.Accounts)
	if len(f.Accounts) != 2 || f.Accounts[0].ID != "default" {
		t.Errorf("accounts = %+v", f.Accounts)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("adopting must not create or touch the default config dir")
	}
}

func TestAdoptDefaultRejectsLoginAlreadyRegistered(t *testing.T) {
	s := newService(t)
	s.AddWithLogin(registry.Account{ID: "a"}, loginAs("me@example.com", "o"))
	_, _, err := s.AdoptDefault("default", registry.Account{Email: "me@example.com", OrgID: "o"}, "/home/.claude")
	if !errors.Is(err, ErrSameLogin) {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultAccountIsNeverPurged(t *testing.T) {
	s := newService(t)
	cfg := t.TempDir()
	marker := filepath.Join(cfg, "precious")
	os.WriteFile(marker, []byte("x"), 0o600)
	s.AdoptDefault("default", registry.Account{Email: "me@example.com"}, cfg)
	if _, err := s.Remove("default", true); !errors.Is(err, ErrDefaultProtected) {
		t.Fatalf("err = %v", err)
	}
	if f, _ := registry.Load(s.Layout.Accounts); len(f.Accounts) != 1 {
		t.Error("refused purge must not unregister")
	}
	if _, err := s.Remove("default", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("unregistering the default account touched ~/.claude")
	}
}

func TestConfigDirFor(t *testing.T) {
	s := newService(t)
	def := registry.Account{ID: "default", Default: true, ConfigDir: "/home/u/.claude"}
	if d, _ := s.ConfigDir(def); d != "/home/u/.claude" {
		t.Errorf("default dir = %q", d)
	}
	if d, _ := s.ConfigDir(registry.Account{ID: "x"}); d != filepath.Join(s.Layout.Root, "profiles", "x") {
		t.Errorf("profile dir = %q", d)
	}
}
