package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDir(t *testing.T) Dir { return Dir(filepath.Join(t.TempDir(), "runtime")) }

func fakeAlive(t *testing.T, alive ...int) {
	t.Helper()
	orig := processAlive
	processAlive = func(pid int) bool {
		for _, p := range alive {
			if p == pid {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { processAlive = orig })
}

func TestRegisterActiveUnregister(t *testing.T) {
	fakeAlive(t, 10, 20)
	d := newDir(t)
	for _, pid := range []int{20, 10, 30} {
		if err := d.Register(Info{PID: pid, Account: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.Active()
	if err != nil || len(got) != 2 || got[0].PID != 10 || got[1].PID != 20 {
		t.Fatalf("Active = %+v, %v (dead pid 30 must be ignored)", got, err)
	}
	d.Unregister(10)
	if got, _ := d.Active(); len(got) != 1 || got[0].PID != 20 {
		t.Errorf("after unregister: %+v", got)
	}
	fi, _ := os.Stat(d.file(20, ".json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestActiveOnMissingDir(t *testing.T) {
	if got, err := newDir(t).Active(); err != nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestRegisterClearsStaleRequestFromReusedPID(t *testing.T) {
	d := newDir(t)
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{".handoff", ".result"} {
		if err := os.WriteFile(d.file(5, s), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Register(Info{PID: 5}); err != nil {
		t.Fatal(err)
	}
	if req, err := d.TakeRequest(5); req != nil || err != nil {
		t.Errorf("stale request survived: %v %v", req, err)
	}
}

func TestSubmitTakeReplyRoundTrip(t *testing.T) {
	fakeAlive(t, 7)
	d := newDir(t)
	go func() {
		for i := 0; i < 200; i++ {
			req, err := d.TakeRequest(7)
			if err != nil {
				t.Error(err)
				return
			}
			if req != nil {
				_ = d.Reply(7, Result{ID: req.ID, OK: true, Message: "to " + req.To})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	res, err := d.Submit(context.Background(), 7, "beta", 5*time.Second)
	if err != nil || !res.OK || res.Message != "to beta" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSubmitIgnoresResultForOtherRequest(t *testing.T) {
	fakeAlive(t, 7)
	d := newDir(t)
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = d.Reply(7, Result{ID: "stale", OK: true})
	}()
	_, err := d.Submit(context.Background(), 7, "b", 400*time.Millisecond)
	if err == nil {
		t.Fatal("a result for a different request id must not be accepted")
	}
}

func TestSubmitTimeoutWithdrawsRequest(t *testing.T) {
	fakeAlive(t, 7)
	d := newDir(t)
	if _, err := d.Submit(context.Background(), 7, "b", 120*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
	if req, _ := d.TakeRequest(7); req != nil {
		t.Error("withdrawn request still pending")
	}
}

func TestSubmitBusyAndDeadProcess(t *testing.T) {
	fakeAlive(t, 7)
	d := newDir(t)
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.file(7, ".handoff"), []byte(`{"id":"x","to":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Submit(context.Background(), 7, "b", time.Second); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v", err)
	}
	if _, err := d.Submit(context.Background(), 99, "b", time.Second); err == nil {
		t.Error("dead pid accepted")
	}
}

func TestTakeRequestMalformedIsConsumed(t *testing.T) {
	d := newDir(t)
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.file(3, ".handoff"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TakeRequest(3); err == nil {
		t.Fatal("expected error")
	}
	if req, err := d.TakeRequest(3); req != nil || err != nil {
		t.Errorf("malformed request not consumed: %v %v", req, err)
	}
}

func TestLockSessionIsExclusive(t *testing.T) {
	d := newDir(t)
	l, err := d.LockSession("sid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.LockSession("sid"); err == nil {
		t.Fatal("second lock on the same session must fail")
	}
	if other, err := d.LockSession("other"); err != nil {
		t.Errorf("different session blocked: %v", err)
	} else {
		_ = other.Release()
	}
	_ = l.Release()
	if again, err := d.LockSession("sid"); err != nil {
		t.Errorf("lock not reusable after release: %v", err)
	} else {
		_ = again.Release()
	}
}

func TestResultSurvivesUnregister(t *testing.T) {
	fakeAlive(t, 7)
	d := newDir(t)
	if err := d.Register(Info{PID: 7}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if req, _ := d.TakeRequest(7); req != nil {
				_ = d.Reply(7, Result{ID: req.ID, Message: "failed"})
				d.Unregister(7)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := d.Submit(context.Background(), 7, "b", 3*time.Second)
	if err != nil || res.Message != "failed" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
