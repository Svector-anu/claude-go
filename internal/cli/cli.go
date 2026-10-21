// Package cli implements the claude-go command line. All I/O and process access is injected.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Svector-anu/claude-go/internal/accounts"
	"github.com/Svector-anu/claude-go/internal/availability"
	"github.com/Svector-anu/claude-go/internal/buildinfo"
	"github.com/Svector-anu/claude-go/internal/claudebin"
	"github.com/Svector-anu/claude-go/internal/config"
	"github.com/Svector-anu/claude-go/internal/control"
	"github.com/Svector-anu/claude-go/internal/hookinstall"
	"github.com/Svector-anu/claude-go/internal/launcher"
	"github.com/Svector-anu/claude-go/internal/memory"
	"github.com/Svector-anu/claude-go/internal/paths"
	"github.com/Svector-anu/claude-go/internal/registry"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	versionTimeout = 5 * time.Second
)

// Env is everything the CLI needs from the outside world.
type Env struct {
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Interactive bool // Stdin is a terminal a person can answer
	Getenv      func(string) string
	UserHome    func() (string, error)
	LookPath    func(string) (string, error)
	Runner      claudebin.Runner
	Now         func() time.Time
	Getwd       func() (string, error)
	Environ     func() []string
	Getpid      func() int
	Executable  func() (string, error)
	// Shell runs a shell command line with stdin, returning its stdout. Used to
	// run the user's own status line command that claude-go wraps.
	Shell   func(ctx context.Context, command string, stdin []byte) ([]byte, error)
	Starter launcher.Starter
	// Signals subscribes to the signals the wrapper forwards to Claude and
	// returns a function that unsubscribes.
	Signals func() (<-chan os.Signal, func())
	// Keys holds the optional memory encryption key. Nil means the platform
	// default: the macOS login keychain, else a private file in the state directory.
	Keys memory.KeyStore
}

const usageText = `claude-go - use several Claude Code accounts as one, and keep your session when one runs out

First time:
  claude-go setup                       register your current login, install the hook
  claude-go accounts add NAME --login   sign in a second account (once per account)

Every day:
  claude-go run [claude arguments]      or just "claude" if setup added the alias

Commands:
  setup [--yes] [--undo] [--memory]
                                        one-time setup (or remove it again)
  run [--account ID] [claude args]      launch Claude Code; switches accounts on a limit
  handoff [--pid PID] ACCOUNT           move a running session to another account now
  accounts list                         accounts and whether each is ready
  accounts add ID [--login] [--label NAME] [--email ADDR]
  accounts login ID                     sign an account in again
  accounts enable ID                    put an account back in rotation
  accounts remove ID [--purge-profile]
  history [-n N]                        recent handoffs and why they happened
  memory list|forget|export|import|path notes Claude saved (see: setup --memory)
  status                                configuration, hook and account state
  version                               print the version

Environment:
  CLAUDE_GO_HOME   state directory (default: ~/.claude-go)
`

// Run executes the command in args (excluding the program name) and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Fprint(env.Stdout, usageText)
		return exitOK
	case "version", "--version", "-v":
		fmt.Fprintf(env.Stdout, "claude-go %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return exitOK
	case "status":
		return runStatus(ctx, args[1:], env)
	case "setup":
		return runSetup(ctx, args[1:], env)
	case "accounts":
		return runAccounts(ctx, args[1:], env)
	case "run":
		return runRun(ctx, args[1:], env)
	case "handoff":
		return runHandoff(ctx, args[1:], env)
	case "hook":
		return runHook(args[1:], env)
	case "statusline":
		return runStatusline(ctx, args[1:], env)
	case "history":
		return runHistory(args[1:], env)
	case "memory":
		return runMemory(args[1:], env)
	case "mcp-memory":
		return runMCPMemory(ctx, env)
	default:
		fmt.Fprintf(env.Stderr, "claude-go: unknown command %q\n\n%s", args[0], usageText)
		return exitUsage
	}
}

func fail(env Env, err error) int {
	fmt.Fprintf(env.Stderr, "claude-go: %v\n", err)
	return exitFailure
}

// app bundles the resolved state locations with the injected environment.
type app struct {
	env    Env
	layout paths.Layout
}

func newApp(env Env) (*app, error) {
	layout, err := newAppLayout(env)
	if err != nil {
		return nil, err
	}
	return &app{env: env, layout: layout}, nil
}

func newAppLayout(env Env) (paths.Layout, error) {
	return paths.Resolve(env.Getenv, env.UserHome)
}

func (a *app) service() accounts.Service {
	return accounts.Service{Layout: a.layout, Now: a.env.Now}
}

func (a *app) claudePath() (string, error) {
	cfg, _, err := config.Load(a.layout.Config)
	if err != nil {
		return "", err
	}
	return claudebin.Locate(cfg.ClaudePath, a.env.LookPath)
}

// launchAccount converts a registered account into something the launcher can run.
func (a *app) launchAccount(acct registry.Account) (launcher.Account, error) {
	dir, err := a.service().ConfigDir(acct)
	if err != nil {
		return launcher.Account{}, err
	}
	return launcher.Account{ID: acct.ID, Label: acct.Label, ConfigDir: dir, Default: acct.Default}, nil
}

func (a *app) hookInstaller() (hookinstall.Installer, error) {
	if a.layout.DefaultClaudeDir == "" {
		return hookinstall.Installer{}, fmt.Errorf("home directory unknown; cannot locate ~/.claude")
	}
	bin, err := a.env.Executable()
	if err != nil {
		return hookinstall.Installer{}, fmt.Errorf("locate claude-go executable: %w", err)
	}
	return hookinstall.Installer{SettingsPath: a.layout.DefaultClaudeDir + "/settings.json", Binary: bin}, nil
}

func runStatus(ctx context.Context, args []string, env Env) int {
	if len(args) > 0 {
		fmt.Fprintf(env.Stderr, "claude-go status: unexpected argument %q\n", args[0])
		return exitUsage
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	cfg, cfgExists, err := config.Load(a.layout.Config)
	if err != nil {
		return fail(env, err)
	}
	reg, err := registry.Load(a.layout.Accounts)
	if err != nil {
		return fail(env, err)
	}
	st, err := availability.Load(a.layout.State)
	if err != nil {
		return fail(env, err)
	}

	fmt.Fprintf(env.Stdout, "claude-go    %s\n", buildinfo.Version)
	fmt.Fprintf(env.Stdout, "state dir    %s\n", a.layout.Root)
	fmt.Fprintf(env.Stdout, "config       %s\n", presence(a.layout.Config, cfgExists))
	fmt.Fprintf(env.Stdout, "claude       %s\n", describeClaude(ctx, cfg, env))
	fmt.Fprintf(env.Stdout, "hook         %s\n", a.describeHook())
	fmt.Fprintf(env.Stdout, "status line  %s\n", a.describeStatusLine())
	fmt.Fprintf(env.Stdout, "mod          %s\n", a.describeMod())
	fmt.Fprintf(env.Stdout, "memory       %s\n", a.describeMemory())
	active, _ := control.Dir(a.layout.Runtime).Active()
	fmt.Fprintf(env.Stdout, "sessions     %d running\n", len(active))
	fmt.Fprintf(env.Stdout, "accounts     %d registered\n", len(reg.Accounts))
	now := env.Now()
	for _, acct := range reg.Accounts {
		fmt.Fprintf(env.Stdout, "  %-12s %s\n", acct.ID, accountState(st, acct.ID, now))
	}
	return exitOK
}

func (a *app) describeHook() string {
	in, err := a.hookInstaller()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	bin, found, err := in.InstalledBinary()
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case !found:
		return "not installed (run: claude-go setup)"
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Sprintf("installed, but %s is missing (run: claude-go setup)", bin)
	}
	return fmt.Sprintf("installed in %s (runs %s)", in.SettingsPath, bin)
}

func (a *app) describeStatusLine() string {
	in, err := a.hookInstaller()
	if err != nil {
		return "unknown"
	}
	on, err := in.StatusLineInstalled()
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case on:
		return "claude-go segment installed"
	default:
		return "not installed (optional: claude-go setup --statusline)"
	}
}

func accountState(st availability.File, id string, now time.Time) string {
	e, blocked := st.Blocked(id, now)
	if !blocked {
		return "ready"
	}
	switch e.Status {
	case availability.Exhausted, availability.ModelLimited:
		label := "at its limit"
		if e.Status == availability.ModelLimited {
			label = "one model at its limit"
		}
		est := ""
		if e.Estimated {
			est = " (estimated)"
		}
		return fmt.Sprintf("%s until %s%s", label, e.Until.Local().Format("Mon 15:04"), est)
	case availability.AuthFailed:
		return "needs sign-in (claude-go accounts login " + id + ")"
	default:
		return "unavailable: billing or account hold"
	}
}

func presence(path string, exists bool) string {
	if exists {
		return path
	}
	return path + " (not created; using defaults)"
}

func describeClaude(ctx context.Context, cfg config.Config, env Env) string {
	path, err := claudebin.Locate(cfg.ClaudePath, env.LookPath)
	if err != nil {
		return "not found (" + err.Error() + ")"
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	version, err := claudebin.Version(ctx, env.Runner, path)
	if err != nil {
		return fmt.Sprintf("%s (version unavailable: %v)", path, err)
	}
	return fmt.Sprintf("%s (%s)", path, version)
}
