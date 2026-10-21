package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return t0 }
	return s
}

// second returns another process's view of the same directory, as another machine.
func second(t *testing.T, dir, machine string, now time.Time) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Machine = machine
	s.Now = func() time.Time { return now }
	return s
}

func mustRemember(t *testing.T, s *Store, scope, key, value string, tags ...string) Fact {
	t.Helper()
	f, err := s.Remember(scope, key, value, tags)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func lines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func TestRememberRecallForgetRoundTrip(t *testing.T) {
	s := open(t)
	f := mustRemember(t, s, GlobalScope, "Coding:Style", "  Prefer small functions.  ", "Style", "go", "style")
	if f.Key != "coding:style" || f.Value != "Prefer small functions." || strings.Join(f.Tags, ",") != "style,go" {
		t.Errorf("normalisation: %+v", f)
	}
	facts, _ := s.Facts()
	if len(facts) != 1 || facts[0].Key != "coding:style" {
		t.Fatalf("facts = %+v", facts)
	}
	if ok, err := s.Forget(GlobalScope, "CODING:STYLE"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	if facts, _ := s.Facts(); len(facts) != 0 {
		t.Errorf("still there: %+v", facts)
	}
	if ok, _ := s.Forget(GlobalScope, "coding:style"); ok {
		t.Error("forgetting twice should report that nothing was there")
	}
}

func TestStoringTheSameThingTwiceWritesNothing(t *testing.T) {
	s := open(t)
	mustRemember(t, s, GlobalScope, "a", "same", "x")
	before := lines(t, s.ownFile())
	mustRemember(t, s, GlobalScope, "a", "same", "x")
	if got := lines(t, s.ownFile()); got != before {
		t.Errorf("log grew from %d to %d lines for an unchanged fact", before, got)
	}
	mustRemember(t, s, GlobalScope, "a", "changed", "x")
	if got := lines(t, s.ownFile()); got != before+1 {
		t.Errorf("a changed value must be appended; lines = %d", got)
	}
}

func TestStateSurvivesReopeningAndFilesArePrivate(t *testing.T) {
	s := open(t)
	mustRemember(t, s, "project:github.com/o/r", "arch", "monolith")
	again, err := Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if facts, _ := again.Facts(); len(facts) != 1 || again.Machine != s.Machine {
		t.Errorf("facts=%+v machine=%q/%q", facts, again.Machine, s.Machine)
	}
	for _, p := range []string{s.Dir, s.ownFile(), filepath.Join(s.Dir, "machine")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", filepath.Base(p), fi.Mode().Perm(), want)
		}
	}
}

func TestInputValidation(t *testing.T) {
	s := open(t)
	long := strings.Repeat("x", MaxValueLen+1)
	cases := map[string]struct {
		scope, key, value string
		tags              []string
	}{
		"empty key":         {GlobalScope, "", "v", nil},
		"key with space":    {GlobalScope, "a b", "v", nil},
		"key with slash-up": {GlobalScope, "../x", "v", nil},
		"long key":          {GlobalScope, strings.Repeat("k", MaxKeyLen+1), "v", nil},
		"empty value":       {GlobalScope, "k", "   ", nil},
		"long value":        {GlobalScope, "k", long, nil},
		"nul in value":      {GlobalScope, "k", "a\x00b", nil},
		"bad scope":         {"everything", "k", "v", nil},
		"scope traversal":   {"project:../..", "k", "v", nil},
		"too many tags":     {GlobalScope, "k", "v", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}},
		"bad tag":           {GlobalScope, "k", "v", []string{"has space"}},
	}
	for name, c := range cases {
		if _, err := s.Remember(c.scope, c.key, c.value, c.tags); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if facts, _ := s.Facts(); len(facts) != 0 {
		t.Errorf("rejected input was stored: %+v", facts)
	}
}

func TestSecretsAreRefusedEverywhereTheyCouldHide(t *testing.T) {
	s := open(t)
	secrets := []string{
		"my key is sk-ant-api03-AbCdEfGhIjKlMnOpQrStUv",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"AWS AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN RSA PRIVATE KEY-----",
		"password: hunter2hunter2",
		"api_key=abcd1234efgh5678",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	}
	for _, sec := range secrets {
		if _, err := s.Remember(GlobalScope, "k", sec, nil); err == nil || !strings.Contains(err.Error(), "looks like") {
			t.Errorf("secret stored in value: %q (err=%v)", sec, err)
		}
	}
	if _, err := s.Remember(GlobalScope, "k", "fine", []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}); err == nil {
		t.Error("a secret hidden in a tag was stored")
	}
	for _, ok := range []string{
		"Password policy: at least 12 characters, rotated yearly",
		"Tokens are JWTs; refresh tokens rotate on use",
		"The API key lives in 1Password under 'prod'",
	} {
		if _, err := s.Remember(GlobalScope, "k2", ok, nil); err != nil {
			t.Errorf("harmless note refused: %q: %v", ok, err)
		}
	}
	for _, p := range []string{s.ownFile()} {
		b, _ := os.ReadFile(p)
		for _, sec := range []string{"hunter2", "AKIAIOSFODNN7", "sk-ant-api03", "BEGIN RSA"} {
			if bytes.Contains(b, []byte(sec)) {
				t.Errorf("a refused secret reached the file: %s", sec)
			}
		}
	}
}

func TestConcurrentWritersFromSeveralHandlesLoseNothing(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	const n = 24
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := Open(s.Dir) // an independent handle, like another Claude session's server
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := h.Remember(GlobalScope, fmt.Sprintf("fact:%d", i), "v", nil); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if facts, _ := s.Facts(); len(facts) != n {
		t.Errorf("facts = %d, want %d", len(facts), n)
	}
	b, _ := os.ReadFile(s.ownFile())
	if got := strings.Count(string(b), "\n"); got != n {
		t.Errorf("log lines = %d, want %d (interleaved or lost writes)", got, n)
	}
}

func TestTheNewestWriteWinsAcrossMachines(t *testing.T) {
	a := open(t)
	b := second(t, a.Dir, "bbbbbbbb", t0.Add(time.Hour))
	mustRemember(t, a, GlobalScope, "db", "postgres")
	mustRemember(t, b, GlobalScope, "db", "sqlite")
	for _, s := range []*Store{a, b} {
		if f, _ := s.Facts(); len(f) != 1 || f[0].Value != "sqlite" {
			t.Errorf("%s sees %+v", s.Machine, f)
		}
	}
	// A later delete on the first machine beats the second machine's write.
	a.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	if ok, _ := a.Forget(GlobalScope, "db"); !ok {
		t.Fatal("forget")
	}
	if f, _ := b.Facts(); len(f) != 0 {
		t.Errorf("deletion did not propagate: %+v", f)
	}
}

func TestTiesBreakTheSameWayOnEveryMachine(t *testing.T) {
	a := open(t)
	b := second(t, a.Dir, "ffffffff", t0) // same instant, larger machine id
	mustRemember(t, a, GlobalScope, "k", "from-a")
	mustRemember(t, b, GlobalScope, "k", "from-b")
	fa, _ := a.Facts()
	fb, _ := b.Facts()
	if fa[0].Value != fb[0].Value || fa[0].Value != "from-b" {
		t.Errorf("a=%q b=%q", fa[0].Value, fb[0].Value)
	}
}

func TestATornLastLineIsIgnoredUntilItIsComplete(t *testing.T) {
	a := open(t)
	mustRemember(t, a, GlobalScope, "one", "1")
	f, _ := os.OpenFile(a.ownFile(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"v":1,"id":"torn","at":"2026-10-20T12:00:00Z","machine":"` + a.Machine + `","op":"upsert","scope":"global","key":"two","value":"2`)
	f.Close()
	r, _ := Open(a.Dir)
	if facts, _ := r.Facts(); len(facts) != 1 {
		t.Fatalf("a half-written line must not be read: %+v", facts)
	}
	f, _ = os.OpenFile(a.ownFile(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("\"}\n")
	f.Close()
	if facts, _ := r.Facts(); len(facts) != 2 {
		t.Errorf("the completed line was not picked up: %+v", facts)
	}
}

func TestDamagedOrHostileLinesFromOtherMachinesAreSkipped(t *testing.T) {
	a := open(t)
	mustRemember(t, a, GlobalScope, "good", "ok")
	bad := strings.Join([]string{
		`not json`,
		`{"v":2,"id":"x","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"global","key":"k","value":"v"}`,
		`{"v":1,"id":"y","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"../../etc","key":"k","value":"v"}`,
		`{"v":1,"id":"z","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"global","key":"has space","value":"v"}`,
		`{"v":1,"id":"w","at":"2026-10-20T12:00:00Z","machine":"m","op":"explode","scope":"global","key":"k"}`,
		`{"v":1,"id":"ok2","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"global","key":"fine","value":"yes"}`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(a.Dir, "m.jsonl"), []byte(bad), 0o600)
	facts, err := a.Facts()
	if err != nil || len(facts) != 2 {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
}

func TestCompactionKeepsFactsAndDeletionsAndOtherHandlesRecover(t *testing.T) {
	a := open(t)
	b := second(t, a.Dir, "bbbbbbbb", t0.Add(-time.Hour))
	mustRemember(t, b, GlobalScope, "doomed", "older write from another machine")
	h, _ := Open(a.Dir) // a second handle on this machine, holding stale state
	h.Now = a.Now
	if f, _ := h.Facts(); len(f) != 1 {
		t.Fatal("setup")
	}

	filler := strings.Repeat("y", MaxValueLen-20)
	for i := 0; i < 700; i++ {
		mustRemember(t, a, GlobalScope, "hot", fmt.Sprintf("%d-%s", i, filler))
	}
	mustRemember(t, a, GlobalScope, "keep", "survives")
	if ok, _ := a.Forget(GlobalScope, "doomed"); !ok {
		t.Fatal("forget")
	}
	for i := 0; i < 700; i++ {
		mustRemember(t, a, GlobalScope, "hot", fmt.Sprintf("again-%d-%s", i, filler))
	}
	fi, _ := os.Stat(a.ownFile())
	if fi.Size() > compactAbove+(3<<20) {
		t.Errorf("log was never compacted: %d bytes", fi.Size())
	}
	for _, s := range []*Store{a, h} {
		facts, _ := s.Facts()
		got := map[string]string{}
		for _, f := range facts {
			got[f.Key] = f.Value
		}
		if len(got) != 2 || got["keep"] != "survives" || !strings.HasPrefix(got["hot"], "again-699-") {
			t.Errorf("after compaction %s sees %d facts: keep=%q", s.Machine, len(got), got["keep"])
		}
		if _, back := got["doomed"]; back {
			t.Error("the deleted fact came back: compaction dropped the tombstone that overrides the other machine's older write")
		}
	}
}

func TestTheFactLimitIsEnforcedButUpdatesStillWork(t *testing.T) {
	s := open(t)
	s.Max = 3
	for i := 0; i < 3; i++ {
		mustRemember(t, s, GlobalScope, fmt.Sprintf("k%d", i), "v")
	}
	if _, err := s.Remember(GlobalScope, "k3", "v", nil); err == nil || !strings.Contains(err.Error(), "full") {
		t.Errorf("err = %v", err)
	}
	if _, err := s.Remember(GlobalScope, "k1", "updated", nil); err != nil {
		t.Errorf("updating an existing fact must work when full: %v", err)
	}
	if ok, _ := s.Forget(GlobalScope, "k0"); !ok {
		t.Fatal("forget")
	}
	if _, err := s.Remember(GlobalScope, "k3", "v", nil); err != nil {
		t.Errorf("space freed by forgetting must be usable: %v", err)
	}
}

func TestExportImportIsIdempotentAndKeepsTheNewestWrite(t *testing.T) {
	src := open(t)
	mustRemember(t, src, GlobalScope, "shared", "from source", "t")
	mustRemember(t, src, "project:github.com/o/r", "arch", "layered")
	src.Now = func() time.Time { return t0.Add(time.Minute) }
	mustRemember(t, src, GlobalScope, "gone", "x")
	src.Forget(GlobalScope, "gone")
	var bundle bytes.Buffer
	if n, err := src.Export(&bundle); err != nil || n != 3 {
		t.Fatalf("export n=%d err=%v", n, err)
	}

	dst := open(t)
	dst.Now = func() time.Time { return t0.Add(time.Hour) }
	mustRemember(t, dst, GlobalScope, "shared", "newer local write") // newer than the bundle's
	res, err := dst.Import(bytes.NewReader(bundle.Bytes()))
	if err != nil || res.Added != 3 || res.Rejected != 0 {
		t.Fatalf("import %+v err=%v", res, err)
	}
	facts, _ := dst.Facts()
	got := map[string]string{}
	for _, f := range facts {
		got[f.Key] = f.Value
	}
	if got["shared"] != "newer local write" || got["arch"] != "layered" {
		t.Errorf("facts = %v", got)
	}
	if _, resurrected := got["gone"]; resurrected {
		t.Error("an imported deletion did not apply")
	}
	again, _ := dst.Import(bytes.NewReader(bundle.Bytes()))
	if again.Added != 0 || again.Skipped != 3 {
		t.Errorf("re-import should add nothing: %+v", again)
	}
	reopened, _ := Open(dst.Dir)
	if f, _ := reopened.Facts(); len(f) != 2 {
		t.Errorf("imported facts did not persist across reopening: %+v", f)
	}
}

func TestImportRefusesMalformedAndSecretBearingLines(t *testing.T) {
	dst := open(t)
	bundle := strings.Join([]string{
		`garbage`,
		`{"v":1,"id":"s1","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"global","key":"leak","value":"password: hunter2hunter2"}`,
		`{"v":1,"id":"g1","at":"2026-10-20T12:00:00Z","machine":"m","op":"upsert","scope":"global","key":"fine","value":"yes"}`,
	}, "\n")
	res, err := dst.Import(strings.NewReader(bundle))
	if err != nil || res.Added != 1 || res.Rejected != 2 {
		t.Errorf("res=%+v err=%v", res, err)
	}
	b, _ := os.ReadFile(filepath.Join(dst.Dir, importFileName))
	if bytes.Contains(b, []byte("hunter2")) {
		t.Error("a secret was written by import")
	}
}

func TestAWriteAfterReadingAFactAlwaysBeatsIt(t *testing.T) {
	a := open(t)
	b := second(t, a.Dir, "bbbbbbbb", t0.Add(24*time.Hour)) // a machine whose clock is a day ahead
	mustRemember(t, b, GlobalScope, "k", "from the machine with the fast clock")
	// a (clock behind) reads it and deliberately replaces it: its write must win.
	mustRemember(t, a, GlobalScope, "k", "the user's correction")
	for _, s := range []*Store{a, b} {
		if f, _ := s.Facts(); f[0].Value != "the user's correction" {
			t.Errorf("%s sees %q: a write made after seeing the old value lost to it", s.Machine, f[0].Value)
		}
	}
	// And a deletion made at the very same instant as the write still wins.
	if ok, _ := a.Forget(GlobalScope, "k"); !ok {
		t.Fatal("forget")
	}
	if f, _ := b.Facts(); len(f) != 0 {
		t.Errorf("deletion lost: %+v", f)
	}
}

func TestNamespacedKeysAreNotMistakenForSecrets(t *testing.T) {
	s := open(t)
	for _, key := range []string{"auth:strategy", "token:rotation", "password:policy", "api_key:location", "secret:storage"} {
		if _, err := s.Remember(GlobalScope, key, "described in the runbook", nil); err != nil {
			t.Errorf("key %q refused: %v", key, err)
		}
	}
	if _, err := s.Remember(GlobalScope, "auth:note", "auth: JWT-with-rotation", nil); err != nil {
		t.Errorf("a description was mistaken for a secret: %v", err)
	}
}
