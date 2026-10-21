package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Svector-anu/claude-go/internal/control"
	"github.com/Svector-anu/claude-go/internal/hookinstall"
	"github.com/Svector-anu/claude-go/internal/launcher"
	"github.com/Svector-anu/claude-go/internal/limits"
)

const maxHookPayload = 1 << 20

// runHook handles `claude-go hook stop-failure`, invoked by Claude Code. It
// must never disturb Claude: it prints nothing to stdout and always exits 0.
// Outside a claude-go session it does nothing at all.
func runHook(args []string, env Env) int {
	if len(args) != 1 || args[0] != hookinstall.Subcommand {
		fmt.Fprintf(env.Stderr, "claude-go hook: unknown hook %v\n", args)
		return exitUsage
	}
	pidText := env.Getenv(launcher.EnvWrapped)
	if pidText == "" {
		return exitOK
	}
	if err := reportFailure(pidText, env); err != nil {
		fmt.Fprintf(env.Stderr, "claude-go hook: %v\n", err)
	}
	return exitOK
}

func reportFailure(pidText string, env Env) error {
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid %s=%q", launcher.EnvWrapped, pidText)
	}
	payload, err := io.ReadAll(io.LimitReader(env.Stdin, maxHookPayload))
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	now := env.Now()
	failure, err := limits.Classify(payload, now, time.Local)
	if err != nil {
		return err
	}
	switch failure.Kind {
	case limits.AccountLimit, limits.ModelLimit, limits.Auth, limits.Blocked:
	default:
		return nil
	}
	var common struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(payload, &common) // already parsed once above; session id is optional
	layout, err := newAppLayout(env)
	if err != nil {
		return err
	}
	return control.Dir(layout.Runtime).PostEvent(pid, control.Event{SessionID: common.SessionID, Failure: failure, At: now, Source: "hook"})
}
