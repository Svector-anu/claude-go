package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteFileAtomicCreatesWithMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := WriteFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestWriteFileAtomicReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for _, c := range []string{"one", "two"} {
		if err := WriteFileAtomic(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p)
	if string(got) != "two" {
		t.Errorf("content = %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestWriteFileAtomicFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := WriteFileAtomic(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Rename onto a non-empty directory fails after the temp file is written.
	target := filepath.Join(dir, "d")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("new"), 0o600); err == nil {
		t.Fatal("expected rename failure")
	}
	if got, _ := os.ReadFile(p); string(got) != "keep" {
		t.Errorf("original damaged: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file not cleaned: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	if err := WriteFileAtomic(filepath.Join(t.TempDir(), "no", "f"), nil, 0o600); err == nil {
		t.Fatal("expected error")
	}
}

func TestLockExcludesAndTimesOut(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	l1, err := AcquireLock(p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(p, 60*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireLock(p, time.Second)
	if err != nil {
		t.Fatalf("lock not reusable after release: %v", err)
	}
	_ = l2.Release()
}

func TestLockSerializesCriticalSection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	var inside, maxInside int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := AcquireLock(p, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			maxInside = max(maxInside, inside)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			_ = l.Release()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("max concurrent holders = %d", maxInside)
	}
}
