// Command claude-go wraps Claude Code with account management and session continuity.
package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/Svector-anu/claude-go/internal/claudebin"
	"github.com/Svector-anu/claude-go/internal/cli"
	"github.com/Svector-anu/claude-go/internal/launcher"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Interactive: isTerminal(os.Stdin),
		Executable:  os.Executable,
		Shell:       runShell,
		Getenv:      os.Getenv,
		UserHome:    os.UserHomeDir,
		LookPath:    exec.LookPath,
		Runner:      claudebin.ExecRunner{},
		Now:         time.Now,
		Getwd:       os.Getwd,
		Environ:     os.Environ,
		Getpid:      os.Getpid,
		Starter:     launcher.ExecStarter{},
		Signals:     forwardedSignals,
	}))
}

// forwardedSignals catches the signals claude-go relays to Claude Code. SIGINT
// is caught too, so Ctrl+C never kills the wrapper; the terminal already
// delivers it to Claude, which owns what Ctrl+C means inside a session.
func forwardedSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return ch, func() { signal.Stop(ch) }
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// runShell runs command through sh with stdin and returns its stdout.
func runShell(ctx context.Context, command string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.Output()
}
