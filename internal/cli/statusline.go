package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"claude-go/internal/history"
	"claude-go/internal/ipc"
	"claude-go/internal/launcher"
	"claude-go/internal/statusline"
)

const (
	statuslineTimeout   = 400 * time.Millisecond
	originalLineTimeout = 2 * time.Second
	statuslineSeparator = " │ "
)

// runStatusline renders claude-go's status line segment. It is installed as
// (or wrapped around) the user's statusLine command, so it must never fail,
// hang, or print an error: a broken status line is worse than a missing one.
func runStatusline(ctx context.Context, args []string, env Env) int {
	var original string
	for i := 0; i < len(args); i++ {
		if args[i] == "--then" && i+1 < len(args) {
			original = args[i+1]
			i++
		}
	}
	input, _ := io.ReadAll(io.LimitReader(env.Stdin, maxHookPayload))

	var theirs string
	if original != "" && env.Shell != nil {
		shellCtx, cancel := context.WithTimeout(ctx, originalLineTimeout)
		out, _ := env.Shell(shellCtx, original, input) // their output is shown even if they exit non-zero
		cancel()
		theirs = strings.TrimRight(string(out), "\r\n")
	}

	ours := ""
	if socket := env.Getenv(launcher.EnvSocket); socket != "" {
		ours = segmentFrom(ctx, socket, statusline.Parse(input), env.Now())
	}
	switch {
	case theirs == "":
		fmt.Fprintln(env.Stdout, ours)
	case ours == "":
		fmt.Fprintln(env.Stdout, theirs)
	default:
		fmt.Fprintln(env.Stdout, theirs+statuslineSeparator+ours)
	}
	return exitOK
}

func segmentFrom(ctx context.Context, socket string, in statusline.Input, now time.Time) string {
	ctx, cancel := context.WithTimeout(ctx, statuslineTimeout)
	defer cancel()
	cl := ipc.NewClient(socket)
	st, err := cl.Status(ctx)
	if err != nil {
		return statusline.Render(in, nil, nil, now)
	}
	accts, err := cl.Accounts(ctx)
	if err != nil {
		accts = nil
	}
	if w := in.Windows(); len(w) > 0 {
		_ = cl.ReportUsage(ctx, w) // best effort: the segment is still correct without it
	}
	return statusline.Render(in, &st, accts, now)
}

// runHistory prints the recent handoff outcomes.
func runHistory(args []string, env Env) int {
	fs := newFlagSet("history", env)
	n := fs.Int("n", 10, "how many entries to show")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	entries, err := history.Tail(a.layout.History, *n)
	if err != nil {
		return fail(env, err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(env.Stdout, "No handoffs yet.")
		return exitOK
	}
	for _, e := range entries {
		fmt.Fprintf(env.Stdout, "%s  %-11s %s -> %s  %s", e.At.Local().Format("Jan 02 15:04"), e.Result, orDash(e.From), orDash(e.To), e.Trigger)
		if e.Reason != "" {
			fmt.Fprintf(env.Stdout, " (%s)", e.Reason)
		}
		fmt.Fprintln(env.Stdout)
	}
	return exitOK
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
