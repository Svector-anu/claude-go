package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func entry(i int) Entry {
	return Entry{At: t0.Add(time.Duration(i) * time.Minute), From: "a", To: "b", Trigger: TriggerAuto, Result: ResultOK, Message: fmt.Sprint("n", i)}
}

func TestAppendAndTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "handoffs.jsonl")
	for i := 0; i < 5; i++ {
		if err := Append(p, entry(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Tail(p, 3)
	if err != nil || len(got) != 3 || got[0].Message != "n2" || got[2].Message != "n4" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if all, _ := Tail(p, 0); len(all) != 5 {
		t.Errorf("n=0 should return everything, got %d", len(all))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestTailMissingFile(t *testing.T) {
	if got, err := Tail(filepath.Join(t.TempDir(), "none"), 5); got != nil || err != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestTornAndGarbageLinesAreSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	Append(p, entry(1))
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("{\"at\":\"2026-10-20T12:00:00Z\",\"trig")
	f.Close()
	if err := Append(p, entry(2)); err != nil {
		t.Fatal(err)
	}
	got, err := Tail(p, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d entries, err=%v", len(got), err)
	}
}

func TestMessageIsClipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	e := entry(1)
	e.Message = strings.Repeat("é", 1000)
	Append(p, e)
	got, _ := Tail(p, 1)
	if n := len([]rune(got[0].Message)); n != maxMessage+1 {
		t.Errorf("message runes = %d", n)
	}
}

func TestCompactionKeepsNewest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	big := strings.Repeat("x", 290)
	for i := 0; i < 1100; i++ {
		e := entry(i)
		e.Message = fmt.Sprintf("%d %s", i, big)
		if err := Append(p, e); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := Tail(p, 0)
	if fi, _ := os.Stat(p); fi.Size() > compactAbove+1024 {
		t.Errorf("log grew to %d bytes; it should stay near %d", fi.Size(), compactAbove)
	}
	if len(all) >= 1100 {
		t.Errorf("nothing was trimmed: %d entries", len(all))
	}
	if !strings.HasPrefix(all[len(all)-1].Message, "1099 ") {
		t.Errorf("newest entry lost: %q", all[len(all)-1].Message[:10])
	}
}

func TestConcurrentAppendsAreAllKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = Append(p, entry(i)) }()
	}
	wg.Wait()
	if got, _ := Tail(p, 0); len(got) != 20 {
		t.Errorf("entries = %d", len(got))
	}
}
