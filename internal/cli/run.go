package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"claude-go/internal/availability"
	"claude-go/internal/claudeargs"
	"claude-go/internal/claudebin"
	"claude-go/internal/config"
	"claude-go/internal/control"
	"claude-go/internal/launcher"
	"claude-go/internal/limits"
	"claude-go/internal/registry"
	"claude-go/internal/transcript"
)

const (
	loginCheckTimeout = 10 * time.Second
	handoffTimeout    = 30 * time.Second
)

const runUsage = `Usage:
  claude-go run [--account ID] [--] [claude arguments...]

Launches Claude Code. Without --account it uses the account you used last, or
the next available one. Arguments after the optional "--" go to claude unchanged.
When the account hits its usage limit the session continues on another account.
`

// splitRunArgs separates claude-go's own leading flag from the arguments owned by claude.
func splitRunArgs(args []string) (account string, rest []string, err error) {
	if len(args) > 0 {
		switch {
		case args[0] == "--account":
			if len(args) < 2 {
				return "", nil, errors.New("--account needs a value")
			}
			account, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--account="):
			account, args = strings.TrimPrefix(args[0], "--account="), args[1:]
		}
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	return account, args, nil
}

func findAccount(reg registry.File, id string) (registry.Account, error) {
	for _, a := range reg.Accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return registry.Account{}, fmt.Errorf("account %q is not registered (see: claude-go accounts list)", id)
}

func runRun(ctx context.Context, args []string, env Env) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(env.Stdout, runUsage)
		return exitOK
	}
	requested, claudeArgs, err := splitRunArgs(args)
	if err != nil {
		fmt.Fprintf(env.Stderr, "claude-go run: %v\n\n%s", err, runUsage)
		return exitUsage
	}
	if env.Getenv(launcher.EnvWrapped) != "" {
		fmt.Fprintln(env.Stderr, "claude-go: refusing to start a nested claude-go session (already running inside one)")
		return exitFailure
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	claudePath, err := a.claudePath()
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

	acct, err := a.pickStartAccount(reg, st, requested)
	if err != nil {
		return fail(env, err)
	}
	plan, err := claudeargs.Build(claudeArgs, claudeargs.NewSessionID)
	if err != nil {
		return fail(env, err)
	}
	cwd, err := env.Getwd()
	if err != nil {
		return fail(env, fmt.Errorf("determine working directory: %w", err))
	}
	la, err := a.launchAccount(acct)
	if err != nil {
		return fail(env, err)
	}
	if !acct.Default {
		profiles := a.service().Profiles()
		if plan.Passthrough {
			_, _, err = profiles.Ensure(acct.ID)
		} else {
			_, _, _, err = profiles.Provision(acct.ID)
		}
		if err != nil {
			return fail(env, err)
		}
	}
	if !plan.Passthrough {
		if err := availability.SetActive(a.layout.State, acct.ID); err != nil {
			return fail(env, err)
		}
	}

	baseEnv := env.Environ()
	sess := &launcher.Session{
		Starter:       env.Starter,
		ClaudePath:    claudePath,
		Plan:          plan,
		Cwd:           cwd,
		Env:           baseEnv,
		Account:       la,
		PID:           env.Getpid(),
		HasTranscript: func(dir, id string) bool { _, ok := transcript.Find(dir, id); return ok },
		Log:           env.Stderr,
	}
	if !plan.Passthrough {
		sess.PluginDir = a.syncMod()
		ctl := newController(a)
		ctl.session = sess
		stopIPC, socket, socketProblem := ctl.startIPC()
		defer stopIPC()
		sess.Socket, sess.SocketErr = socket, socketProblem
		sess.Requests = ctl.requests
		sess.OnOutcome = ctl.record
		sess.Control = control.Dir(a.layout.Runtime)
		sess.Prepare = func(ctx context.Context, id string) (launcher.Account, error) {
			return a.prepareTarget(ctx, claudePath, baseEnv, id)
		}
		sess.Now = env.Now
		watcher := &tailWatcher{app: a, sessionID: plan.SessionID}
		sess.Detect = watcher.detect
		sess.Auto = func(ctx context.Context, ev control.Event, cur launcher.Account) launcher.AutoDecision {
			return a.autoDecide(ctx, claudePath, baseEnv, ev, cur, sess.Plan.SessionID, sess.ChildStartedAt())
		}
		sess.OnSwitch = func(next launcher.Account) {
			if err := availability.SetActive(a.layout.State, next.ID); err != nil {
				fmt.Fprintf(env.Stderr, "claude-go: could not remember active account: %v\n", err)
			}
		}
	}
	signals, stop := env.Signals()
	defer stop()
	sess.Signals = signals

	code, err := sess.Run(ctx)
	if err != nil {
		return fail(env, err)
	}
	return code
}

// pickStartAccount honours --account, otherwise chooses by availability.
func (a *app) pickStartAccount(reg registry.File, st availability.File, requested string) (registry.Account, error) {
	if requested != "" {
		return findAccount(reg, requested)
	}
	if len(reg.Accounts) == 0 {
		return registry.Account{}, errors.New("no accounts registered (run: claude-go setup)")
	}
	acct, exhausted, err := availability.Start(reg.Accounts, st, a.env.Now())
	if err != nil {
		return registry.Account{}, err
	}
	if exhausted {
		if _, at, ok := availability.EarliestReset(reg.Accounts, st, a.env.Now()); ok {
			fmt.Fprintf(a.env.Stderr, "All accounts are at their limit; starting on %s, which resets %s\n", nameOf(acct), a.resetPhrase(at))
		} else {
			fmt.Fprintf(a.env.Stderr, "No account is currently available; starting on %s anyway (see: claude-go status)\n", nameOf(acct))
		}
	}
	return acct, nil
}

func nameOf(a registry.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

func (a *app) resetPhrase(at time.Time) string {
	now := a.env.Now()
	local := at.Local()
	clock := local.Format("15:04")
	if local.YearDay() != now.Local().YearDay() || local.Year() != now.Local().Year() {
		clock = local.Format("Mon 15:04")
	}
	return fmt.Sprintf("at %s (in %s)", clock, at.Sub(now).Round(time.Minute))
}

// prepareTarget validates a handoff target before the running session is disturbed.
func (a *app) prepareTarget(ctx context.Context, claudePath string, baseEnv []string, id string) (launcher.Account, error) {
	reg, err := registry.Load(a.layout.Accounts)
	if err != nil {
		return launcher.Account{}, err
	}
	acct, err := findAccount(reg, id)
	if err != nil {
		return launcher.Account{}, err
	}
	la, err := a.launchAccount(acct)
	if err != nil {
		return launcher.Account{}, err
	}
	if !acct.Default {
		if _, _, _, err := a.service().Profiles().Provision(id); err != nil {
			return launcher.Account{}, err
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, loginCheckTimeout)
	defer cancel()
	ident, err := claudebin.WhoAmI(checkCtx, a.env.Runner, launcher.ProfileEnv(baseEnv, la), claudePath)
	if err != nil {
		return launcher.Account{}, fmt.Errorf("could not verify login: %w", err)
	}
	if !ident.LoggedIn {
		return launcher.Account{}, fmt.Errorf("account %q is not signed in (run: claude-go accounts login %s)", id, id)
	}
	return la, nil
}

// autoDecide reacts to a failure reported by the Claude hook: it records why the
// current account is out of rotation and picks the next usable one.
func (a *app) autoDecide(ctx context.Context, claudePath string, baseEnv []string, ev control.Event, cur launcher.Account, sessionID string, since time.Time) launcher.AutoDecision {
	now := a.env.Now()
	entry := availability.Entry{Since: now, Reason: ev.Failure.Message, Until: ev.Failure.ResetAt, Estimated: ev.Failure.Estimated}
	var verb, reason string
	switch ev.Failure.Kind {
	case limits.AccountLimit:
		entry.Status, verb, reason = availability.Exhausted, "hit its limit", "usage limit"
	case limits.ModelLimit:
		entry.Status, verb, reason = availability.ModelLimited, "hit its limit", "model limit"
	case limits.Auth:
		entry.Status, verb, reason = availability.AuthFailed, "needs to sign in again", "sign-in required"
	case limits.Blocked:
		entry.Status, verb, reason = availability.Blocked, "is unavailable (billing or account hold)", "billing or account hold"
	default:
		return launcher.AutoDecision{}
	}
	isLimit := ev.Failure.Kind == limits.AccountLimit || ev.Failure.Kind == limits.ModelLimit
	// A limit Claude recorded at the end of the transcript confirms the turn was cut short.
	// A hook can arrive a moment before the transcript line is flushed, so give it a short grace.
	var recorded *transcript.Limit
	if isLimit {
		recorded = a.awaitRecordedLimit(cur, sessionID, since, ev.Source == "transcript")
	}
	confirmed := ev.Source == "transcript" || recorded != nil

	if entry.Estimated || entry.Until.IsZero() {
		// Most to least trustworthy: the limit Claude recorded in the transcript,
		// then the last window it reported, then text parsing, then a guess.
		if recorded != nil && !recorded.ResetsAt.IsZero() {
			entry.Until, entry.Estimated = recorded.ResetsAt, false
			reason += " (" + windowName(recorded.Type) + ")"
		} else if st, err := availability.Load(a.layout.State); err == nil {
			if at, ok := st.VerifiedReset(cur.ID, now); ok {
				entry.Until, entry.Estimated = at, false
			}
		}
	} else if ev.Source == "transcript" {
		if name := windowName(typeFromMessage(ev)); name != "" && isLimit {
			reason += " (" + name + ")"
		}
	}
	if err := availability.Mark(a.layout.State, cur.ID, entry); err != nil {
		return launcher.AutoDecision{Reason: reason, Note: fmt.Sprintf("could not record that %s is unavailable: %v", cur.Name(), err)}
	}

	reg, err := registry.Load(a.layout.Accounts)
	if err != nil {
		return launcher.AutoDecision{Reason: reason, Note: err.Error()}
	}
	for range reg.Accounts {
		st, err := availability.Load(a.layout.State)
		if err != nil {
			return launcher.AutoDecision{Reason: reason, Note: err.Error()}
		}
		next, ok := availability.Next(reg.Accounts, st, now, cur.ID)
		if !ok {
			break
		}
		target, err := a.prepareTarget(ctx, claudePath, baseEnv, next.ID)
		if err != nil {
			_ = availability.Mark(a.layout.State, next.ID, availability.Entry{
				Status: availability.AuthFailed, Since: now, Reason: err.Error(),
			})
			continue
		}
		d := launcher.AutoDecision{
			Target: &target,
			Line:   fmt.Sprintf("Account %s %s, continuing on Account %s", cur.Name(), verb, target.Name()),
			Reason: reason,
		}
		if confirmed && isLimit {
			if cfg, _, err := config.Load(a.layout.Config); err == nil {
				d.Continue = cfg.ContinuePrompt()
			}
		}
		return d
	}
	return launcher.AutoDecision{Reason: reason, Note: a.noAccountLeft(reg, cur, verb, now)}
}

func (a *app) noAccountLeft(reg registry.File, cur launcher.Account, verb string, now time.Time) string {
	st, err := availability.Load(a.layout.State)
	if err == nil {
		if id, at, ok := availability.EarliestReset(reg.Accounts, st, now); ok {
			if acct, err := findAccount(reg, id); err == nil {
				return fmt.Sprintf("Account %s %s and no other account is available. Earliest reset: %s %s.", cur.Name(), verb, nameOf(acct), a.resetPhrase(at))
			}
		}
	}
	return fmt.Sprintf("Account %s %s and no other account is available. Sign in another with: claude-go accounts add NAME --login", cur.Name(), verb)
}

const handoffUsage = `Usage:
  claude-go handoff <account> [--pid PID]

Asks a running claude-go session to continue the same conversation under
another registered account.
`

func runHandoff(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("handoff", env)
	pid := fs.Int("pid", 0, "pid of the claude-go session (required if several are running)")
	to, code, ok := parseWithID(fs, args, env)
	if !ok {
		if code == exitUsage {
			fmt.Fprint(env.Stderr, handoffUsage)
		}
		return code
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	dir := control.Dir(a.layout.Runtime)
	target := *pid
	if target == 0 {
		active, err := dir.Active()
		if err != nil {
			return fail(env, err)
		}
		switch len(active) {
		case 0:
			return fail(env, errors.New("no running claude-go session (start one with: claude-go run)"))
		case 1:
			target = active[0].PID
		default:
			var b strings.Builder
			for _, s := range active {
				fmt.Fprintf(&b, "\n  pid %d  account %s  %s", s.PID, s.Account, s.Cwd)
			}
			return fail(env, fmt.Errorf("several sessions running; choose one with --pid:%s", b.String()))
		}
	}
	res, err := dir.Submit(ctx, target, to, handoffTimeout)
	if err != nil {
		return fail(env, err)
	}
	if !res.OK {
		fmt.Fprintf(env.Stderr, "claude-go: handoff failed: %s\n", res.Message)
		return exitFailure
	}
	fmt.Fprintf(env.Stdout, "%s\n", res.Message)
	return exitOK
}

// typeFromMessage names the window a transcript-sourced event came from.
func typeFromMessage(ev control.Event) string {
	switch {
	case strings.Contains(ev.Failure.Message, "weekly"):
		return "seven_day"
	case strings.Contains(ev.Failure.Message, "session"):
		return "five_hour"
	default:
		return ""
	}
}
