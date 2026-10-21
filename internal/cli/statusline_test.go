package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Svector-anu/claude-go/internal/ipc"
)

type statusCtrl struct {
	mu    sync.Mutex
	usage [][]ipc.Window
}

func (c *statusCtrl) Status() ipc.Status {
	return ipc.Status{Session: ipc.SessionView{Account: ipc.AccountRef{ID: "a", Label: "Work"}, Phase: "running"}}
}
func (c *statusCtrl) Accounts() ([]ipc.AccountView, error) {
	return []ipc.AccountView{{ID: "a", Current: true}, {ID: "b", State: ipc.StateReady}}, nil
}
func (c *statusCtrl) History(int) ([]ipc.HistoryEntry, error) { return nil, nil }
func (c *statusCtrl) RequestHandoff(context.Context, string) (<-chan struct{}, error) {
	return nil, errors.New("no")
}
func (c *statusCtrl) RecordUsage(w []ipc.Window) error {
	c.mu.Lock()
	c.usage = append(c.usage, w)
	c.mu.Unlock()
	return nil
}

func serve(t *testing.T) (string, *statusCtrl) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c := &statusCtrl{}
	srv := ipc.NewServer(c, nil)
	sock := filepath.Join(dir, "s.sock")
	if err := srv.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return sock, c
}

func (h *harness) statusline(t *testing.T, socket, stdin string, args ...string) string {
	t.Helper()
	env := h.env
	env.Stdin = strings.NewReader(stdin)
	env.Getenv = func(k string) string {
		if k == "CLAUDE_GO_SOCKET" {
			return socket
		}
		return h.vars[k]
	}
	h.stdout.Reset()
	if code := Run(context.Background(), append([]string{"statusline"}, args...), env); code != exitOK {
		t.Fatalf("statusline must always exit 0, got %d", code)
	}
	return h.stdout.String()
}

const sampleInput = `{"session_id":"s1","rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":1738425600}}}`

func TestStatuslineShowsAccountUsageAndOthers(t *testing.T) {
	h := newHarness(t)
	sock, ctrl := serve(t)
	out := h.statusline(t, sock, sampleInput)
	if out != "⇄ Work · 5h 24% · 1 other ready\n" {
		t.Errorf("out = %q", out)
	}
	if len(ctrl.usage) != 1 || ctrl.usage[0][0].Kind != "five_hour" || ctrl.usage[0][0].PercentUsed != 23.5 {
		t.Errorf("usage was not reported to the controller: %+v", ctrl.usage)
	}
}

func TestStatuslineWrapsTheUsersCommandWithoutReplacingIt(t *testing.T) {
	h := newHarness(t)
	sock, _ := serve(t)
	var gotCmd, gotStdin string
	h.env.Shell = func(_ context.Context, cmd string, stdin []byte) ([]byte, error) {
		gotCmd, gotStdin = cmd, string(stdin)
		return []byte("[Opus] 8% context\n"), nil
	}
	out := h.statusline(t, sock, sampleInput, "--then", "their-script.sh")
	if gotCmd != "their-script.sh" || gotStdin != sampleInput {
		t.Errorf("their command ran as %q with stdin %q", gotCmd, gotStdin)
	}
	if out != "[Opus] 8% context │ ⇄ Work · 5h 24% · 1 other ready\n" {
		t.Errorf("out = %q", out)
	}
}

func TestStatuslineKeepsTheUsersOutputWhenTheirCommandFailsOrIsSlow(t *testing.T) {
	h := newHarness(t)
	sock, _ := serve(t)
	h.env.Shell = func(context.Context, string, []byte) ([]byte, error) {
		return []byte("partial output"), errors.New("exit status 1")
	}
	if out := h.statusline(t, sock, "{}", "--then", "x"); !strings.HasPrefix(out, "partial output │ ") {
		t.Errorf("out = %q", out)
	}
}

func TestStatuslineOutsideClaudeGoPrintsOnlyTheUsersLine(t *testing.T) {
	h := newHarness(t)
	h.env.Shell = func(context.Context, string, []byte) ([]byte, error) { return []byte("theirs\n"), nil }
	if out := h.statusline(t, "", sampleInput, "--then", "x"); out != "theirs\n" {
		t.Errorf("out = %q", out)
	}
	if out := h.statusline(t, "", sampleInput); strings.TrimSpace(out) != "" {
		t.Errorf("without claude-go there is nothing to add, got %q", out)
	}
}

func TestStatuslineSurvivesAnUnreachableControllerAndGarbageInput(t *testing.T) {
	h := newHarness(t)
	dead := filepath.Join(os.TempDir(), "cg-no-such.sock")
	if out := h.statusline(t, dead, sampleInput); !strings.Contains(out, "unreachable") {
		t.Errorf("out = %q", out)
	}
	sock, _ := serve(t)
	if out := h.statusline(t, sock, "not json at all"); !strings.HasPrefix(out, "⇄ Work") {
		t.Errorf("out = %q", out)
	}
}

func TestStatuslineIsFastWhenTheControllerHangs(t *testing.T) {
	h := newHarness(t)
	dir, _ := os.MkdirTemp("", "cg")
	defer os.RemoveAll(dir)
	// A listener that accepts but never answers.
	sock := filepath.Join(dir, "hang.sock")
	hang := listenAndHang(t, sock)
	defer hang()
	start := time.Now()
	out := h.statusline(t, sock, sampleInput)
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s; a status line must not stall the UI", time.Since(start))
	}
	if !strings.Contains(out, "unreachable") {
		t.Errorf("out = %q", out)
	}
}

func TestSetupStatuslineWrapsAndUndoRestores(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	if err := os.MkdirAll(filepath.Dir(h.settingsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := `{"statusLine":{"type":"command","command":"my-line.sh","padding":1}}`
	os.WriteFile(h.settingsPath(), []byte(orig), 0o600)

	if code := h.run("setup", "--yes", "--statusline"); code != exitOK {
		t.Fatalf("code=%d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	got := readFile(t, h.settingsPath())
	if !strings.Contains(got, "statusline --then") || !strings.Contains(got, "my-line.sh") || !strings.Contains(got, `"padding"`) {
		t.Errorf("settings = %s", got)
	}
	h.stdout.Reset()
	h.run("status")
	if !strings.Contains(h.stdout.String(), "claude-go segment installed") {
		t.Errorf("status:\n%s", h.stdout.String())
	}
	if code := h.run("setup", "--undo"); code != exitOK {
		t.Fatalf("undo code=%d", code)
	}
	after := readFile(t, h.settingsPath())
	if !strings.Contains(after, `"command": "my-line.sh"`) || strings.Contains(after, "claude-go") {
		t.Errorf("not restored: %s", after)
	}
}

func TestSetupStatusLineIsRecommendedButOptOutable(t *testing.T) {
	h := newHarness(t)
	h.env.Runner = identityRunner(h, nil)
	h.run("setup", "--yes", "--no-statusline")
	if strings.Contains(readFile(t, h.settingsPath()), "statusLine") {
		t.Error("--no-statusline must leave the status line alone")
	}
	h2 := newHarness(t)
	h2.env.Runner = identityRunner(h2, nil)
	h2.run("setup")
	if strings.Contains(readFile(t, h2.settingsPath()), "statusLine") {
		t.Error("a non-interactive setup without --yes must not touch the status line")
	}
	h3 := newHarness(t)
	h3.env.Runner = identityRunner(h3, nil)
	h3.run("setup", "--yes")
	if !strings.Contains(readFile(t, h3.settingsPath()), "statusline") {
		t.Error("--yes should add the status line segment")
	}
}

func TestHistoryCommand(t *testing.T) {
	h := newHarness(t)
	h.run("history")
	if !strings.Contains(h.stdout.String(), "No handoffs yet") {
		t.Errorf("out = %q", h.stdout.String())
	}
	h.writeState(t, "handoffs.jsonl", `{"at":"2026-10-20T10:00:00Z","from":"a","to":"b","trigger":"auto","reason":"usage limit","result":"ok"}`+"\n")
	h.stdout.Reset()
	h.run("history")
	out := h.stdout.String()
	if !strings.Contains(out, "a -> b") || !strings.Contains(out, "auto (usage limit)") {
		t.Errorf("out = %q", out)
	}
}
