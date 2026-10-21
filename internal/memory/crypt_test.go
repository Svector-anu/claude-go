package memory

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSealOpenRoundTripAndTamperDetection(t *testing.T) {
	c := testCipher(t)
	line, err := c.Seal([]byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !isSealed(line) || bytes.Contains(line, []byte("hello")) {
		t.Fatalf("not sealed: %s", line)
	}
	got, err := c.Open(line)
	if err != nil || string(got) != `{"hello":"world"}` {
		t.Fatalf("open = %q, %v", got, err)
	}
	tampered := []byte(strings.Replace(string(line), `"c":"`, `"c":"AAAA`, 1))
	if _, err := c.Open(tampered); err == nil {
		t.Error("a changed line opened")
	}
	if _, err := testCipher(t).Open(line); err == nil {
		t.Error("a line opened with another key")
	}
	again, _ := c.Seal([]byte(`{"hello":"world"}`))
	if bytes.Equal(line, again) {
		t.Error("the same text sealed to the same bytes (nonce reused)")
	}
}

func TestNewCipherRejectsAShortKey(t *testing.T) {
	if _, err := NewCipher([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func logText(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(s.ownFile())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnEncryptedStoreKeepsNotesOutOfTheFileAndStillRecallsThem(t *testing.T) {
	s := open(t)
	s.Cipher = testCipher(t)
	mustRemember(t, s, GlobalScope, "db:orm", "Drizzle with PostgreSQL", "database")
	text := logText(t, s)
	for _, leak := range []string{"Drizzle", "PostgreSQL", "db:orm", "database"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q is readable in the log:\n%s", leak, text)
		}
	}
	if got := mustRecall(t, s, Query{Topic: "postgresql"}); len(got) != 1 || got[0].Key != "db:orm" {
		t.Errorf("recall = %v", keysOf(got))
	}
	// a fresh store with the same key sees the note; one without a key sees it locked
	reopened, _ := Open(s.Dir)
	reopened.Cipher = s.Cipher
	if facts, _ := reopened.Facts(); len(facts) != 1 {
		t.Errorf("reopened with the key: %d facts", len(facts))
	}
	keyless, _ := Open(s.Dir)
	if facts, _ := keyless.Facts(); len(facts) != 0 {
		t.Errorf("without the key: %d facts", len(facts))
	}
	if n, _ := keyless.Locked(); n != 1 {
		t.Errorf("locked = %d, want 1", n)
	}
	wrong, _ := Open(s.Dir)
	wrong.Cipher = testCipher(t)
	if n, _ := wrong.Locked(); n != 1 {
		t.Errorf("locked with a wrong key = %d, want 1", n)
	}
}

func TestPlainAndSealedLinesCanShareALog(t *testing.T) {
	s := open(t)
	mustRemember(t, s, GlobalScope, "old:plain", "written before encryption")
	s.Cipher = testCipher(t)
	mustRemember(t, s, GlobalScope, "new:sealed", "written after")
	facts, _ := s.Facts()
	if len(facts) != 2 {
		t.Fatalf("facts = %v", facts)
	}
	text := logText(t, s)
	if !strings.Contains(text, "old:plain") || strings.Contains(text, "new:sealed") {
		t.Errorf("log:\n%s", text)
	}
}

func TestRewriteEncryptsEverythingThenDecryptsItBack(t *testing.T) {
	s := open(t)
	mustRemember(t, s, GlobalScope, "a:one", "first note")
	mustRemember(t, s, proj, "b:two", "second note")
	if _, err := s.Forget(GlobalScope, "a:one"); err != nil {
		t.Fatal(err)
	}
	c := testCipher(t)
	if err := s.Rewrite(c); err != nil {
		t.Fatal(err)
	}
	if text := logText(t, s); strings.Contains(text, "second note") || strings.Contains(text, "a:one") {
		t.Errorf("plain text left after Rewrite:\n%s", text)
	}
	if facts, _ := s.Facts(); len(facts) != 1 || facts[0].Key != "b:two" {
		t.Errorf("after encrypting: %v", facts)
	}
	if err := s.Rewrite(nil); err != nil {
		t.Fatal(err)
	}
	if text := logText(t, s); !strings.Contains(text, "second note") || strings.Contains(text, `"enc"`) {
		t.Errorf("not plain after decrypting:\n%s", text)
	}
	if facts, _ := s.Facts(); len(facts) != 1 || facts[0].Key != "b:two" {
		t.Errorf("after decrypting: %v (the deletion must survive)", facts)
	}
}

func TestRewriteRefusesToLoseNotesItCannotOpen(t *testing.T) {
	s := open(t)
	s.Cipher = testCipher(t)
	mustRemember(t, s, GlobalScope, "keep:me", "precious")
	before := logText(t, s)
	wrongKey := testCipher(t)
	other, _ := Open(s.Dir)
	other.Cipher = wrongKey
	if err := other.Rewrite(nil); err == nil {
		t.Fatal("rewrite with the wrong key succeeded")
	}
	if logText(t, s) != before {
		t.Error("the log changed although the rewrite failed")
	}
}

func TestEncryptedCompactionAndImportStaySealed(t *testing.T) {
	src := open(t)
	mustRemember(t, src, GlobalScope, "moved:note", "from another machine")
	var bundle bytes.Buffer
	if _, err := src.Export(&bundle); err != nil {
		t.Fatal(err)
	}
	dst := open(t)
	dst.Cipher = testCipher(t)
	if res, err := dst.Import(&bundle); err != nil || res.Added != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	b, err := os.ReadFile(filepath.Join(dst.Dir, importFileName))
	if err != nil || strings.Contains(string(b), "from another machine") {
		t.Errorf("imported text is readable on disk: %s (%v)", b, err)
	}
	if facts, _ := dst.Facts(); len(facts) != 1 {
		t.Errorf("imported fact lost: %v", facts)
	}
}

func TestFileKeyStore(t *testing.T) {
	ks := FileKeyStore{Path: filepath.Join(t.TempDir(), "sub", "memory.key")}
	if _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty store: %v", err)
	}
	key, _ := GenerateKey()
	if err := ks.Save(key); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Load()
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load = %x, %v", got, err)
	}
	if fi, _ := os.Stat(ks.Path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v", fi.Mode().Perm())
	}
	if err := ks.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := ks.Delete(); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	os.WriteFile(ks.Path, []byte("not hex"), 0o600)
	if _, err := ks.Load(); err == nil || errors.Is(err, ErrNoKey) {
		t.Errorf("damaged key: %v", err)
	}
}

// fakeSecurity stands in for /usr/bin/security.
type fakeSecurity struct {
	items map[string]string
	calls [][]string
	stdin []string
}

func (f *fakeSecurity) run(stdin string, args ...string) (string, int, error) {
	f.calls = append(f.calls, args)
	f.stdin = append(f.stdin, stdin)
	switch args[0] {
	case "-i":
		fields := strings.Fields(stdin)
		f.items[fields[3]+"/"+fields[5]] = fields[7]
		return "", 0, nil
	case "find-generic-password":
		v, ok := f.items[args[2]+"/"+args[4]]
		if !ok {
			return "", keychainItemNotFound, nil
		}
		return v + "\n", 0, nil
	case "delete-generic-password":
		if _, ok := f.items[args[2]+"/"+args[4]]; !ok {
			return "", keychainItemNotFound, nil
		}
		delete(f.items, args[2]+"/"+args[4])
	}
	return "", 0, nil
}

func TestKeychainKeyStoreNeverPutsTheKeyInArguments(t *testing.T) {
	fake := &fakeSecurity{items: map[string]string{}}
	ks := KeychainKeyStore{Service: "svc", Account: "acct", run: fake.run}
	if _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty: %v", err)
	}
	key, _ := GenerateKey()
	if err := ks.Save(key); err != nil {
		t.Fatal(err)
	}
	for _, args := range fake.calls {
		for _, a := range args {
			if strings.Contains(a, hexOf(key)) {
				t.Fatalf("the key was passed as an argument: %v", args)
			}
		}
	}
	got, err := ks.Load()
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load = %x, %v", got, err)
	}
	if err := ks.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Errorf("after delete: %v", err)
	}
	if err := ks.Delete(); err != nil {
		t.Errorf("deleting a missing item: %v", err)
	}
}

func TestKeychainSaveReportsAFailureThatSecurityDoesNotExitWith(t *testing.T) {
	failing := func(stdin string, args ...string) (string, int, error) {
		if args[0] == "find-generic-password" {
			return "", keychainItemNotFound, nil
		}
		return "", 0, nil // `security -i` swallowed an error
	}
	ks := KeychainKeyStore{Service: "svc", Account: "acct", run: failing}
	key, _ := GenerateKey()
	if err := ks.Save(key); err == nil {
		t.Fatal("a save that did not stick was reported as done")
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// Opt-in: touches the real macOS login keychain with a throwaway item.
//
//	CLAUDE_GO_REAL_KEYCHAIN=1 go test ./internal/memory -run RealKeychain -v
func TestRealKeychain(t *testing.T) {
	if os.Getenv("CLAUDE_GO_REAL_KEYCHAIN") != "1" {
		t.Skip("set CLAUDE_GO_REAL_KEYCHAIN=1 to use the real keychain")
	}
	ks := NewKeychainKeyStore(fmt.Sprintf("claude-go-memory-test-%d", os.Getpid()), "key")
	defer ks.Delete()
	if _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("fresh item: %v", err)
	}
	key, _ := GenerateKey()
	if err := ks.Save(key); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Load()
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load = %x, %v", got, err)
	}
	other, _ := GenerateKey()
	if err := ks.Save(other); err != nil { // -U updates in place
		t.Fatal(err)
	}
	if got, _ := ks.Load(); !bytes.Equal(got, other) {
		t.Error("update did not replace the key")
	}
	if err := ks.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Load(); !errors.Is(err, ErrNoKey) {
		t.Errorf("after delete: %v", err)
	}
}
