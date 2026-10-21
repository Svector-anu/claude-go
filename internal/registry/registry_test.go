package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingIsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 0 || f.Version != currentVersion {
		t.Errorf("unexpected: %+v", f)
	}
}

func TestLoadValid(t *testing.T) {
	f, err := Load(write(t, `{"version":1,"accounts":[{"id":"work","label":"Work","email":"a@b.co","created_at":"2026-10-20T00:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].ID != "work" {
		t.Errorf("unexpected: %+v", f)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"truncated":      `{"version":1,"accounts":[{"id":"a"`,
		"empty file":     ``,
		"future version": `{"version":9,"accounts":[]}`,
		"unknown field":  `{"version":1,"accounts":[],"tokens":[]}`,
		"missing id":     `{"version":1,"accounts":[{"label":"x"}]}`,
		"duplicate id":   `{"version":1,"accounts":[{"id":"a"},{"id":"a"}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := write(t, content)
			_, err := Load(p)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error should name the file: %v", err)
			}
		})
	}
}

func TestLoadRejectsTraversalIDs(t *testing.T) {
	for _, id := range []string{"../evil", "a/b", ".hidden", "UPPER", "", "has space"} {
		p := write(t, `{"version":1,"accounts":[{"id":"`+id+`"}]}`)
		if _, err := Load(p); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

func TestSaveRoundTripAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	p := filepath.Join(root, "accounts.json")
	in := File{Accounts: []Account{{ID: "a", Label: "A"}}}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil || len(out.Accounts) != 1 || out.Accounts[0].ID != "a" {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(root)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", di.Mode().Perm())
	}
}

func TestMutateErrorWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "accounts.json")
	if err := Save(p, File{Accounts: []Account{{ID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	err := Mutate(p, func(f *File) error {
		f.Accounts = nil
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("expected error")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("file changed despite fn error")
	}
}

func TestMutateNeverOverwritesCorruptState(t *testing.T) {
	p := write(t, `{"version":1,"accounts":[{"id":"a"`)
	called := false
	err := Mutate(p, func(*File) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	got, _ := os.ReadFile(p)
	if string(got) != `{"version":1,"accounts":[{"id":"a"` {
		t.Errorf("corrupt file was modified: %q", got)
	}
}

func TestLeftoverTempFileFromCrashIsIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "accounts.json")
	if err := Save(p, File{Accounts: []Account{{ID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(filepath.Dir(p), ".accounts.json.tmp-123")
	if err := os.WriteFile(stray, []byte(`{"version":1,"accounts":[{"id":"half`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil || len(f.Accounts) != 1 {
		t.Fatalf("load with stray temp: %+v %v", f, err)
	}
	if err := Mutate(p, func(f *File) error { f.Accounts = append(f.Accounts, Account{ID: "b"}); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentMutateLosesNoUpdates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "accounts.json")
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("acct-%d", i)
			if err := Mutate(p, func(f *File) error {
				f.Accounts = append(f.Accounts, Account{ID: id})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != n {
		t.Errorf("accounts = %d, want %d (lost updates)", len(f.Accounts), n)
	}
}

func TestLoadValidatesDefaultAccounts(t *testing.T) {
	cases := map[string]string{
		"relative config dir": `{"version":1,"accounts":[{"id":"d","default":true,"config_dir":"rel"}]}`,
		"missing config dir":  `{"version":1,"accounts":[{"id":"d","default":true}]}`,
		"two defaults":        `{"version":1,"accounts":[{"id":"a","default":true,"config_dir":"/a"},{"id":"b","default":true,"config_dir":"/b"}]}`,
	}
	for name, content := range cases {
		if _, err := Load(write(t, content)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSameLogin(t *testing.T) {
	a := Account{Email: "x@y.z", OrgID: "o"}
	if !SameLogin(a, Account{Email: "x@y.z", OrgID: "o"}) {
		t.Error("identical logins should match")
	}
	if SameLogin(a, Account{Email: "x@y.z", OrgID: "other"}) || SameLogin(Account{}, Account{}) {
		t.Error("distinct or unknown logins must not match")
	}
}
