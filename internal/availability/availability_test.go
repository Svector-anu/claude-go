package availability

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claude-go/internal/registry"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func accts(ids ...string) []registry.Account {
	out := make([]registry.Account, len(ids))
	for i, id := range ids {
		out[i] = registry.Account{ID: id}
	}
	return out
}

func exhaustedUntil(d time.Duration) Entry {
	return Entry{Status: Exhausted, Until: t0.Add(d), Since: t0}
}

func TestNextRotatesAfterCurrent(t *testing.T) {
	a := accts("a", "b", "c")
	cases := []struct {
		current string
		st      File
		want    string
		ok      bool
	}{
		{"a", File{}, "b", true},
		{"c", File{}, "a", true},
		{"a", File{Accounts: map[string]Entry{"b": exhaustedUntil(time.Hour)}}, "c", true},
		{"a", File{Accounts: map[string]Entry{"b": exhaustedUntil(-time.Minute)}}, "b", true}, // reset has passed
		{"a", File{Accounts: map[string]Entry{"b": exhaustedUntil(time.Hour), "c": exhaustedUntil(time.Hour)}}, "", false},
		{"a", File{Accounts: map[string]Entry{"b": {Status: AuthFailed}, "c": {Status: Blocked}}}, "", false},
		{"unknown", File{}, "a", true},
	}
	for i, c := range cases {
		got, ok := Next(a, c.st, t0, c.current)
		if ok != c.ok || got.ID != c.want {
			t.Errorf("case %d: got %q,%v want %q,%v", i, got.ID, ok, c.want, c.ok)
		}
	}
}

func TestNextNeverReturnsCurrent(t *testing.T) {
	if got, ok := Next(accts("a"), File{}, t0, "a"); ok {
		t.Errorf("returned the only (current) account: %v", got)
	}
}

func TestNextPrefersFullyAvailableOverModelLimited(t *testing.T) {
	st := File{Accounts: map[string]Entry{"b": {Status: ModelLimited, Until: t0.Add(time.Hour)}}}
	if got, _ := Next(accts("a", "b", "c"), st, t0, "a"); got.ID != "c" {
		t.Errorf("got %q, want c", got.ID)
	}
	st.Accounts["c"] = exhaustedUntil(time.Hour)
	if got, ok := Next(accts("a", "b", "c"), st, t0, "a"); !ok || got.ID != "b" {
		t.Errorf("model-limited account should be the last resort: %q %v", got.ID, ok)
	}
}

func TestStart(t *testing.T) {
	a := accts("a", "b")
	if got, ex, err := Start(a, File{Active: "b"}, t0); err != nil || ex || got.ID != "b" {
		t.Errorf("active available: %v %v %v", got.ID, ex, err)
	}
	if got, ex, _ := Start(a, File{Active: "a", Accounts: map[string]Entry{"a": exhaustedUntil(time.Hour)}}, t0); ex || got.ID != "b" {
		t.Errorf("active exhausted: %v %v", got.ID, ex)
	}
	if got, ex, _ := Start(a, File{}, t0); ex || got.ID != "a" {
		t.Errorf("no history should start at the first account: %v %v", got.ID, ex)
	}
	all := File{Active: "a", Accounts: map[string]Entry{"a": exhaustedUntil(3 * time.Hour), "b": exhaustedUntil(time.Hour)}}
	if got, ex, _ := Start(a, all, t0); !ex || got.ID != "b" {
		t.Errorf("all exhausted should pick the earliest reset: %v %v", got.ID, ex)
	}
	if _, _, err := Start(nil, File{}, t0); err == nil {
		t.Error("expected error with no accounts")
	}
}

func TestEarliestReset(t *testing.T) {
	st := File{Accounts: map[string]Entry{
		"a": exhaustedUntil(3 * time.Hour),
		"b": exhaustedUntil(time.Hour),
		"c": {Status: AuthFailed},
		"d": exhaustedUntil(-time.Hour),
	}}
	id, at, ok := EarliestReset(accts("a", "b", "c", "d"), st, t0)
	if !ok || id != "b" || !at.Equal(t0.Add(time.Hour)) {
		t.Errorf("got %q %v %v", id, at, ok)
	}
	if _, _, ok := EarliestReset(accts("c"), st, t0); ok {
		t.Error("auth failures have no reset time")
	}
}

func TestPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	if err := Mark(p, "a", exhaustedUntil(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(p, "a"); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil || f.Active != "a" || f.Accounts["a"].Status != Exhausted {
		t.Fatalf("%+v %v", f, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if err := Clear(p, "a"); err != nil {
		t.Fatal(err)
	}
	if f, _ := Load(p); len(f.Accounts) != 0 {
		t.Errorf("not cleared: %+v", f)
	}
}

func TestLoadCorruptAndVersion(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"corrupt": "{x", "future": `{"version":9}`, "unknown": `{"version":1,"tokens":1}`} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("%s accepted", name)
		}
		if err := Mark(p, "a", Entry{}); err == nil {
			t.Errorf("%s: Mark must not overwrite bad state", name)
		}
		if got, _ := os.ReadFile(p); string(got) != content {
			t.Errorf("%s: file modified", name)
		}
	}
}

func TestConcurrentMarksAreNotLost(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	ids := []string{"a", "b", "c", "d", "e", "f"}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Mark(p, id, exhaustedUntil(time.Hour)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f, _ := Load(p); len(f.Accounts) != len(ids) {
		t.Errorf("accounts = %d, want %d", len(f.Accounts), len(ids))
	}
}

func TestVerifiedReset(t *testing.T) {
	seen := t0.Add(-10 * time.Minute)
	f := File{Usage: map[string]UsageSeen{
		"a": {SeenAt: seen, Windows: []Window{
			{Kind: "five_hour", PercentUsed: 98, ResetsAt: t0.Add(2 * time.Hour)},
			{Kind: "seven_day", PercentUsed: 96, ResetsAt: t0.Add(30 * time.Hour)},
		}},
		"low":   {SeenAt: seen, Windows: []Window{{Kind: "five_hour", PercentUsed: 60, ResetsAt: t0.Add(time.Hour)}}},
		"past":  {SeenAt: seen, Windows: []Window{{Kind: "five_hour", PercentUsed: 99, ResetsAt: t0.Add(-time.Minute)}}},
		"stale": {SeenAt: t0.Add(-7 * time.Hour), Windows: []Window{{Kind: "five_hour", PercentUsed: 99, ResetsAt: t0.Add(time.Hour)}}},
		"noset": {SeenAt: seen, Windows: []Window{{Kind: "five_hour", PercentUsed: 99}}},
	}}
	if at, ok := f.VerifiedReset("a", t0); !ok || !at.Equal(t0.Add(30*time.Hour)) {
		t.Errorf("all spent windows must reset: got %v %v", at, ok)
	}
	for _, id := range []string{"low", "past", "stale", "noset", "unknown"} {
		if _, ok := f.VerifiedReset(id, t0); ok {
			t.Errorf("%s: should not date a reset", id)
		}
	}
}

func TestRecordUsagePersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	u := UsageSeen{SeenAt: t0, Windows: []Window{{Kind: "five_hour", PercentUsed: 12.5}}}
	if err := RecordUsage(p, "a", u); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil || f.Usage["a"].Windows[0].PercentUsed != 12.5 {
		t.Fatalf("%+v %v", f, err)
	}
}
