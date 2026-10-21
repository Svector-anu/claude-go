package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCtrl struct {
	mu        sync.Mutex
	handoffs  []string
	usage     [][]Window
	reject    error
	block     bool
	finished  chan struct{}
	historyN  int
	accounts  []AccountView
	status    Status
	failUsage error
}

func (f *fakeCtrl) Status() Status                   { return f.status }
func (f *fakeCtrl) Accounts() ([]AccountView, error) { return f.accounts, nil }
func (f *fakeCtrl) History(n int) ([]HistoryEntry, error) {
	f.historyN = n
	return nil, nil
}
func (f *fakeCtrl) RecordUsage(w []Window) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, w)
	return f.failUsage
}
func (f *fakeCtrl) RequestHandoff(ctx context.Context, to string) (<-chan struct{}, error) {
	f.mu.Lock()
	f.handoffs = append(f.handoffs, to)
	ch, rej, block := f.finished, f.reject, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if rej != nil {
		return nil, rej
	}
	return ch, nil
}
func (f *fakeCtrl) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.handoffs) }

// shortDir keeps socket paths under the OS limit (t.TempDir paths can be too long).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "cg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func start(t *testing.T, c *fakeCtrl) (*Client, string, *Server) {
	t.Helper()
	if c.finished == nil {
		c.finished = make(chan struct{})
	}
	path := filepath.Join(shortDir(t), "run", "s.sock")
	srv := NewServer(c, nil)
	srv.AcceptTimeout = 300 * time.Millisecond
	if err := srv.Listen(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return NewClient(path), path, srv
}

func rawPost(t *testing.T, sock, path, contentType string, body []byte) (int, ErrorBody) {
	t.Helper()
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(http.MethodPost, "http://x"+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	return resp.StatusCode, eb
}

func TestReadEndpoints(t *testing.T) {
	c := &fakeCtrl{
		status:   Status{APIVersion: APIVersion, PID: 42, Version: "v1", Session: SessionView{Phase: "running", Account: AccountRef{ID: "a", Label: "A"}}},
		accounts: []AccountView{{ID: "a", Label: "A", State: StateReady, Current: true}},
	}
	cl, _, _ := start(t, c)
	ctx := context.Background()
	h, err := cl.Health(ctx)
	if err != nil || !h.OK || h.PID != 42 || h.APIVersion != 1 {
		t.Fatalf("health = %+v, %v", h, err)
	}
	st, err := cl.Status(ctx)
	if err != nil || st.Session.Phase != "running" || st.Session.Account.ID != "a" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	accts, err := cl.Accounts(ctx)
	if err != nil || len(accts) != 1 || accts[0].State != StateReady {
		t.Fatalf("accounts = %+v, %v", accts, err)
	}
	if h, err := cl.History(ctx, 7); err != nil || h == nil && false {
		t.Fatalf("history: %v", err)
	}
	if c.historyN != 7 {
		t.Errorf("limit passed = %d", c.historyN)
	}
}

func TestHistoryLimitValidation(t *testing.T) {
	cl, _, _ := start(t, &fakeCtrl{})
	for _, n := range []int{0, -1, 201} {
		if _, err := cl.History(context.Background(), n); err == nil {
			t.Errorf("limit %d accepted", n)
		}
	}
}

func TestSocketAndDirectoryArePrivate(t *testing.T) {
	_, path, _ := start(t, &fakeCtrl{})
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", di.Mode().Perm())
	}
}

func TestMethodAndPathErrors(t *testing.T) {
	cl, path, _ := start(t, &fakeCtrl{})
	_ = cl
	if code, eb := rawPost(t, path, "/v1/status", "application/json", []byte("{}")); code != 405 || eb.Error.Code != CodeMethod {
		t.Errorf("POST status: %d %+v", code, eb)
	}
	httpc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
	for _, c := range []struct {
		url  string
		want int
	}{{"http://x/v1/handoff", 405}, {"http://x/v1/nope", 404}, {"http://x/v2/status", 404}} {
		resp, err := httpc.Get(c.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("GET %s = %d, want %d", c.url, resp.StatusCode, c.want)
		}
	}
}

func TestMalformedRequestsAreRejectedWithoutReachingTheController(t *testing.T) {
	c := &fakeCtrl{}
	_, path, _ := start(t, c)
	good := `{"request_id":"req-12345678","to":"beta"}`
	cases := []struct {
		name, ct, body string
		want           int
		code           string
	}{
		{"not json", "application/json", `nope`, 400, CodeInvalid},
		{"empty", "application/json", ``, 400, CodeInvalid},
		{"unknown field", "application/json", `{"request_id":"req-12345678","to":"beta","admin":true}`, 400, CodeInvalid},
		{"trailing data", "application/json", good + `{"x":1}`, 400, CodeInvalid},
		{"wrong content type", "text/plain", good, 415, CodeInvalid},
		{"short request id", "application/json", `{"request_id":"abc","to":"beta"}`, 400, CodeInvalid},
		{"bad request id chars", "application/json", `{"request_id":"req 12345678!","to":"beta"}`, 400, CodeInvalid},
		{"path traversal account", "application/json", `{"request_id":"req-12345678","to":"../etc"}`, 400, CodeInvalid},
		{"empty account", "application/json", `{"request_id":"req-12345678","to":""}`, 400, CodeInvalid},
		{"oversized", "application/json", `{"request_id":"req-12345678","to":"` + strings.Repeat("a", 70<<10) + `"}`, 413, CodeTooLarge},
	}
	for _, tc := range cases {
		code, eb := rawPost(t, path, "/v1/handoff", tc.ct, []byte(tc.body))
		if code != tc.want || eb.Error.Code != tc.code {
			t.Errorf("%s: got %d %+v, want %d %s", tc.name, code, eb.Error, tc.want, tc.code)
		}
	}
	if c.calls() != 0 {
		t.Errorf("controller reached %d times by invalid requests", c.calls())
	}
}

func TestUsageValidation(t *testing.T) {
	c := &fakeCtrl{}
	cl, path, _ := start(t, c)
	reset := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	if err := cl.ReportUsage(context.Background(), []Window{{Kind: "five_hour", PercentUsed: 23.5, ResetsAt: &reset}, {Kind: "seven_day", PercentUsed: 41}}); err != nil {
		t.Fatal(err)
	}
	if len(c.usage) != 1 || len(c.usage[0]) != 2 || c.usage[0][0].PercentUsed != 23.5 {
		t.Errorf("usage = %+v", c.usage)
	}
	if err := cl.ReportUsage(context.Background(), nil); err != nil {
		t.Errorf("an empty report is valid (no windows yet): %v", err)
	}
	bad := []string{
		`{"windows":[{"kind":"hourly","percent_used":1}]}`,
		`{"windows":[{"kind":"five_hour","percent_used":-1}]}`,
		`{"windows":[{"kind":"five_hour","percent_used":5000}]}`,
		`{"windows":[{"kind":"five_hour","percent_used":1,"token":"x"}]}`,
		`{"windows":[` + strings.TrimSuffix(strings.Repeat(`{"kind":"five_hour","percent_used":1},`, 9), ",") + `]}`,
	}
	before := len(c.usage)
	for _, b := range bad {
		if code, _ := rawPost(t, path, "/v1/usage", "application/json", []byte(b)); code != 400 {
			t.Errorf("%s: code %d", b, code)
		}
	}
	if len(c.usage) != before {
		t.Error("invalid usage reached the controller")
	}
}

func TestHandoffAcceptedAndIdempotentReplay(t *testing.T) {
	c := &fakeCtrl{}
	cl, _, _ := start(t, c)
	ctx := context.Background()
	a, err := cl.Handoff(ctx, "req-aaaaaaaa", "beta")
	if err != nil || !a.Accepted || a.To != "beta" || a.Replayed {
		t.Fatalf("first = %+v, %v", a, err)
	}
	again, err := cl.Handoff(ctx, "req-aaaaaaaa", "beta")
	if err != nil || !again.Replayed {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if c.calls() != 1 {
		t.Errorf("controller called %d times for one request id", c.calls())
	}
}

func TestSecondHandoffIsBusyUntilTheFirstFinishes(t *testing.T) {
	c := &fakeCtrl{}
	cl, _, _ := start(t, c)
	ctx := context.Background()
	if _, err := cl.Handoff(ctx, "req-aaaaaaaa", "beta"); err != nil {
		t.Fatal(err)
	}
	_, err := cl.Handoff(ctx, "req-bbbbbbbb", "gamma")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Detail.Code != CodeBusy || ae.Detail.RequestID != "req-aaaaaaaa" {
		t.Fatalf("err = %v", err)
	}
	close(c.finished)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := cl.Handoff(ctx, "req-cccccccc", "gamma"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot never released after the handoff finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRejectedHandoffIsReportedAndReplayedWithoutRecalling(t *testing.T) {
	c := &fakeCtrl{reject: errors.New("account \"beta\" is not signed in")}
	cl, _, _ := start(t, c)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := cl.Handoff(ctx, "req-aaaaaaaa", "beta")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 422 || ae.Detail.Code != CodeRejected || !strings.Contains(ae.Detail.Message, "not signed in") {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if c.calls() != 1 {
		t.Errorf("controller called %d times", c.calls())
	}
	// A rejection frees the slot: a different request may proceed.
	c.mu.Lock()
	c.reject = nil
	c.mu.Unlock()
	if _, err := cl.Handoff(ctx, "req-bbbbbbbb", "gamma"); err != nil {
		t.Errorf("slot not released after a rejection: %v", err)
	}
}

func TestHandoffTimesOutWhenTheControllerHangs(t *testing.T) {
	c := &fakeCtrl{block: true}
	cl, _, _ := start(t, c)
	start := time.Now()
	_, err := cl.Handoff(context.Background(), "req-aaaaaaaa", "beta")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 504 || ae.Detail.Code != CodeTimeout {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestConcurrentHandoffsAdmitExactlyOne(t *testing.T) {
	c := &fakeCtrl{}
	cl, _, _ := start(t, c)
	var ok, busy atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "req-" + strings.Repeat(string(rune('a'+i)), 8)
			_, err := cl.Handoff(context.Background(), id, "beta")
			var ae *APIError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &ae) && ae.Detail.Code == CodeBusy:
				busy.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || busy.Load() != 11 || c.calls() != 1 {
		t.Errorf("ok=%d busy=%d controllerCalls=%d", ok.Load(), busy.Load(), c.calls())
	}
}

func TestUnavailableControllerIsAClearClientError(t *testing.T) {
	cl := NewClient(filepath.Join(shortDir(t), "none.sock"))
	if _, err := cl.Health(context.Background()); err == nil {
		t.Fatal("expected a connection error")
	}
}

func TestListenReplacesStaleSocketButNeverStealsALiveOne(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "s.sock")
	// A dead process leaves its socket file behind.
	ln, _ := net.Listen("unix", path)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("setup: stale socket missing")
	}
	live := NewServer(&fakeCtrl{}, nil)
	if err := live.Listen(path); err != nil {
		t.Fatalf("stale socket was not replaced: %v", err)
	}
	defer live.Close()
	if err := NewServer(&fakeCtrl{}, nil).Listen(path); err == nil {
		t.Fatal("a second server stole a live socket")
	}
}

func TestCloseRemovesSocket(t *testing.T) {
	_, path, srv := start(t, &fakeCtrl{})
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("socket file left behind")
	}
}

func TestOverlongSocketPathIsRefused(t *testing.T) {
	long := filepath.Join(shortDir(t), strings.Repeat("d", 120), "s.sock")
	if err := NewServer(&fakeCtrl{}, nil).Listen(long); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("err = %v", err)
	}
}
