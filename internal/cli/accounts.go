package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	"claude-go/internal/accounts"
	"claude-go/internal/availability"
	"claude-go/internal/claudebin"
	"claude-go/internal/launcher"
	"claude-go/internal/registry"
)

const accountsUsage = `Usage:
  claude-go accounts list
  claude-go accounts add <id> [--login] [--label NAME] [--email ADDRESS]
  claude-go accounts login <id>
  claude-go accounts enable <id>
  claude-go accounts remove <id> [--purge-profile]
`

func runAccounts(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, accountsUsage)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return runAccountsList(args[1:], env)
	case "add":
		return runAccountsAdd(ctx, args[1:], env)
	case "login":
		return runAccountsLogin(ctx, args[1:], env)
	case "enable":
		return runAccountsEnable(args[1:], env)
	case "remove":
		return runAccountsRemove(args[1:], env)
	default:
		fmt.Fprintf(env.Stderr, "claude-go accounts: unknown subcommand %q\n\n%s", args[0], accountsUsage)
		return exitUsage
	}
}

func newFlagSet(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// parseWithID parses flags that may appear before or after the single positional
// account id, so both `add --login work` and `add work --login` work.
func parseWithID(fs *flag.FlagSet, args []string, env Env) (id string, code int, ok bool) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "", exitOK, false
			}
			return "", exitUsage, false
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fmt.Fprintf(env.Stderr, "claude-go %s: expected exactly one account id\n", fs.Name())
		return "", exitUsage, false
	}
	return positional[0], exitOK, true
}

func runAccountsList(args []string, env Env) int {
	if len(args) > 0 {
		fmt.Fprintf(env.Stderr, "claude-go accounts list: unexpected argument %q\n", args[0])
		return exitUsage
	}
	a, err := newApp(env)
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
	if len(reg.Accounts) == 0 {
		fmt.Fprintln(env.Stdout, "No accounts registered. Run: claude-go setup")
		return exitOK
	}
	now := env.Now()
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tSTATE\tLIMITS (last seen)")
	for _, acct := range reg.Accounts {
		name := nameOf(acct)
		if acct.Default {
			name += " (default)"
		} else if name != acct.ID {
			name += " (" + acct.ID + ")"
		}
		limits := "no reading yet"
		if u, ok := st.Usage[acct.ID]; ok {
			limits = limitsSummary(u, now)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, accountState(st, acct.ID, now), limits)
	}
	if err := tw.Flush(); err != nil {
		return fail(env, fmt.Errorf("write output: %w", err))
	}
	return exitOK
}

// signIn runs the real `claude auth login` against acct's profile and returns
// who ended up logged in. Nothing here ever sees a credential.
func (a *app) signIn(ctx context.Context, claudePath string, acct launcher.Account, emailHint string) (registry.Account, error) {
	cwd, err := a.env.Getwd()
	if err != nil {
		return registry.Account{}, fmt.Errorf("determine working directory: %w", err)
	}
	childEnv := launcher.ProfileEnv(a.env.Environ(), acct)
	args := []string{"auth", "login"}
	if emailHint != "" {
		args = append(args, "--email", emailHint)
	}
	fmt.Fprintf(a.env.Stdout, "Signing in %s - finish in the browser window Claude opens.\n", acct.Name())
	proc, err := a.env.Starter.Start(launcher.Spec{Path: claudePath, Args: args, Dir: cwd, Env: childEnv})
	if err != nil {
		return registry.Account{}, fmt.Errorf("start claude login: %w", err)
	}
	code, err := proc.Wait()
	if err != nil {
		return registry.Account{}, err
	}
	if code != 0 {
		return registry.Account{}, fmt.Errorf("claude login exited with status %d", code)
	}
	ident, err := claudebin.WhoAmI(ctx, a.env.Runner, childEnv, claudePath)
	if err != nil {
		return registry.Account{}, fmt.Errorf("could not verify the login: %w", err)
	}
	if !ident.LoggedIn {
		return registry.Account{}, errors.New("the login did not complete")
	}
	return registry.Account{Email: ident.Email, OrgID: ident.OrgID}, nil
}

func runAccountsAdd(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("accounts add", env)
	label := fs.String("label", "", "display name (defaults to the email once signed in, else the id)")
	email := fs.String("email", "", "email to pre-fill on the sign-in page")
	login := fs.Bool("login", false, "sign the account in now")
	id, code, ok := parseWithID(fs, args, env)
	if !ok {
		return code
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	svc := a.service()
	acct := registry.Account{ID: id, Label: *label, Email: *email}

	var res accounts.AddResult
	if *login {
		claudePath, err := a.claudePath()
		if err != nil {
			return fail(env, err)
		}
		res, err = svc.AddWithLogin(acct, func(dir string) (registry.Account, error) {
			ident, err := a.signIn(ctx, claudePath, launcher.Account{ID: id, Label: *label, ConfigDir: dir}, *email)
			ident.Label = ident.Email
			return ident, err
		})
		if err != nil {
			if errors.Is(err, accounts.ErrSameLogin) {
				fmt.Fprintf(env.Stderr, "claude-go: %v\nThe browser signed in as an account that is already registered. Choose the other account on the sign-in page.\n", err)
				return exitFailure
			}
			return fail(env, err)
		}
	} else {
		res, err = svc.Add(acct)
		if err != nil {
			return fail(env, err)
		}
	}
	fmt.Fprintf(env.Stdout, "Registered account %q.\n", id)
	if res.ReusedProfile {
		fmt.Fprintf(env.Stdout, "Reusing existing profile directory (contents untouched): %s\n", res.ProfileDir)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(env.Stdout, "warning: %s\n", w)
	}
	if !*login {
		fmt.Fprintf(env.Stdout, "Not signed in yet. Run: claude-go accounts login %s\n", id)
	}
	return exitOK
}

func runAccountsLogin(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("accounts login", env)
	id, code, ok := parseWithID(fs, args, env)
	if !ok {
		return code
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	reg, err := registry.Load(a.layout.Accounts)
	if err != nil {
		return fail(env, err)
	}
	var acct registry.Account
	found := false
	for _, r := range reg.Accounts {
		if r.ID == id {
			acct, found = r, true
		}
	}
	if !found {
		return fail(env, fmt.Errorf("%w: %q", accounts.ErrNotFound, id))
	}
	claudePath, err := a.claudePath()
	if err != nil {
		return fail(env, err)
	}
	if !acct.Default {
		if _, _, _, err := a.service().Profiles().Provision(id); err != nil {
			return fail(env, err)
		}
	}
	la, err := a.launchAccount(acct)
	if err != nil {
		return fail(env, err)
	}
	ident, err := a.signIn(ctx, claudePath, la, acct.Email)
	if err != nil {
		return fail(env, err)
	}
	err = registry.Mutate(a.layout.Accounts, func(f *registry.File) error {
		for i := range f.Accounts {
			if f.Accounts[i].ID == id {
				continue
			}
			if registry.SameLogin(f.Accounts[i], ident) {
				return fmt.Errorf("%w: it is account %q (%s)", accounts.ErrSameLogin, f.Accounts[i].ID, f.Accounts[i].Email)
			}
		}
		for i := range f.Accounts {
			if f.Accounts[i].ID == id {
				f.Accounts[i].Email, f.Accounts[i].OrgID = ident.Email, ident.OrgID
			}
		}
		return nil
	})
	if err != nil {
		return fail(env, err)
	}
	if err := availability.Clear(a.layout.State, id); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Account %q is signed in as %s and back in rotation.\n", id, ident.Email)
	return exitOK
}

func runAccountsEnable(args []string, env Env) int {
	fs := newFlagSet("accounts enable", env)
	id, code, ok := parseWithID(fs, args, env)
	if !ok {
		return code
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	if err := availability.Clear(a.layout.State, id); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Account %q is back in rotation.\n", id)
	return exitOK
}

func runAccountsRemove(args []string, env Env) int {
	fs := newFlagSet("accounts remove", env)
	purge := fs.Bool("purge-profile", false, "also delete the profile directory and any credentials in it")
	id, code, ok := parseWithID(fs, args, env)
	if !ok {
		return code
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	res, err := a.service().Remove(id, *purge)
	if err != nil {
		return fail(env, err)
	}
	if err := availability.Clear(a.layout.State, id); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "Unregistered account %q.\n", id)
	switch {
	case res.Purged:
		fmt.Fprintf(env.Stdout, "Deleted profile directory: %s\n", res.ProfileDir)
	case strings.HasSuffix(res.ProfileDir, "/.claude"):
		fmt.Fprintf(env.Stdout, "Your own Claude config was not touched: %s\n", res.ProfileDir)
	default:
		fmt.Fprintf(env.Stdout, "Profile directory kept (may contain credentials): %s\n", res.ProfileDir)
		fmt.Fprintln(env.Stdout, "Re-run with --purge-profile to delete it.")
	}
	return exitOK
}
