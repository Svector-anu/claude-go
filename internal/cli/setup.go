package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"claude-go/internal/accounts"
	"claude-go/internal/claudebin"
	"claude-go/internal/fsx"
	"claude-go/internal/hookinstall"
	"claude-go/internal/launcher"
	"claude-go/internal/registry"
	"claude-go/internal/shellrc"
)

const (
	defaultAccountID = "default"
	installRelDir    = ".local/bin"
	binaryName       = "claude-go"
)

type setup struct {
	*app
	ctx    context.Context
	yes    bool
	reader *bufio.Reader
	home   string

	statusLine bool
	mod        bool
	install    bool
	alias      bool
	memory     bool
}

func runSetup(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("setup", env)
	yes := fs.Bool("yes", false, "do the recommended setup without asking")
	undo := fs.Bool("undo", false, "remove the hook, status line segment, companion and shortcut (accounts and profiles are kept)")
	withStatusLine := fs.Bool("statusline", false, "add claude-go's segment to your status line even without asking")
	withMod := fs.Bool("mod", false, "install the /accounts, /switch, /flow companion even without asking")
	noInstall := fs.Bool("no-install", false, "do not copy claude-go to ~/.local/bin")
	noStatusLine := fs.Bool("no-statusline", false, "leave your status line alone")
	noMod := fs.Bool("no-mod", false, "do not install the /accounts, /switch, /flow companion")
	noAlias := fs.Bool("no-alias", false, "do not add the `claude` shortcut to your shell config")
	withMemory := fs.Bool("memory", false, "let Claude save and recall project notes that follow you across accounts and machines")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "claude-go setup: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	home, err := env.UserHome()
	if err != nil || a.layout.DefaultClaudeDir == "" {
		return fail(env, errors.New("cannot determine your home directory"))
	}
	s := &setup{app: a, ctx: ctx, yes: *yes, reader: bufio.NewReader(env.Stdin), home: home,
		statusLine: *withStatusLine, mod: *withMod, memory: *withMemory}
	if *undo {
		return s.undo()
	}
	return s.run(wishes{noInstall: *noInstall, noStatusLine: *noStatusLine, noMod: *noMod, noAlias: *noAlias})
}

// wishes are the opt-outs from the recommended setup.
type wishes struct{ noInstall, noStatusLine, noMod, noAlias bool }

// Result symbols shown at the start of each line.
const (
	symOK   = "✓"
	symWarn = "!"
	symSkip = "–"
	symFail = "✗"
)

const labelWidth = 14

func (s *setup) row(sym, label, format string, args ...any) {
	fmt.Fprintf(s.env.Stdout, "  %s %-*s %s\n", sym, labelWidth, label, fmt.Sprintf(format, args...))
}

// tilde shortens a path under the home directory.
func (s *setup) tilde(p string) string {
	if rest, ok := strings.CutPrefix(p, s.home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

func (s *setup) confirm(question string, def bool) bool {
	if s.yes {
		return true
	}
	if !s.env.Interactive {
		return false
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(s.env.Stdout, "%s %s ", question, hint)
	line, _ := s.reader.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

// decide settles which optional parts to set up. With --yes or one "yes" to a
// single question, everything recommended is included (minus any --no-*);
// otherwise only what was asked for with flags. ok is false when the user declined.
func (s *setup) decide(w wishes) (ok bool) {
	if !s.yes && s.env.Interactive {
		fmt.Fprintln(s.env.Stdout, "claude-go setup")
		fmt.Fprintln(s.env.Stdout)
		fmt.Fprintln(s.env.Stdout, "This will:")
		fmt.Fprintln(s.env.Stdout, "  · register your current Claude login (nothing is copied or re-entered)")
		fmt.Fprintf(s.env.Stdout, "  · add a hook to %s so a usage limit can trigger a switch\n", s.tilde(filepath.Join(s.layout.DefaultClaudeDir, "settings.json")))
		if !w.noInstall {
			fmt.Fprintf(s.env.Stdout, "  · copy claude-go to ~/%s\n", installRelDir)
		}
		if !w.noStatusLine {
			fmt.Fprintln(s.env.Stdout, "  · add an account segment to your status line (your own stays)")
		}
		if !w.noMod {
			fmt.Fprintln(s.env.Stdout, "  · add /accounts, /switch and /flow inside Claude")
		}
		if !w.noAlias {
			fmt.Fprintln(s.env.Stdout, "  · make `claude` start claude-go (a block in your shell config)")
		}
		fmt.Fprintln(s.env.Stdout, "Everything is reversible with: claude-go setup --undo")
		fmt.Fprintln(s.env.Stdout)
		if !s.confirm("Continue?", true) {
			fmt.Fprintln(s.env.Stdout, "Nothing changed.")
			return false
		}
		fmt.Fprintln(s.env.Stdout)
		s.install, s.alias = !w.noInstall, !w.noAlias
		s.statusLine, s.mod = s.statusLine || !w.noStatusLine, s.mod || !w.noMod
		return true
	}
	if s.yes {
		fmt.Fprintln(s.env.Stdout, "claude-go setup")
		fmt.Fprintln(s.env.Stdout)
		s.install, s.alias = !w.noInstall, !w.noAlias
		s.statusLine, s.mod = s.statusLine || !w.noStatusLine, s.mod || !w.noMod
		return true
	}
	// Not interactive and not --yes: change only what flags asked for.
	fmt.Fprintln(s.env.Stdout, "claude-go setup")
	fmt.Fprintln(s.env.Stdout)
	return true
}

func (s *setup) run(w wishes) int {
	claudePath, err := s.claudePath()
	if err != nil {
		return fail(s.env, err)
	}
	if !s.decide(w) {
		return exitOK
	}
	baseEnv := s.env.Environ()

	s.adoptDefaultLogin(claudePath, baseEnv)
	binary := s.installBinary()
	hookOK := s.installHook(binary)
	s.repairProfiles(claudePath, baseEnv)
	if s.statusLine {
		s.installStatusLine(binary)
	}
	if s.mod {
		s.installMod(claudePath)
	}
	if s.memory {
		s.installMemory(binary)
	}
	s.shellBlock(binary)

	reg, err := registry.Load(s.layout.Accounts)
	if err != nil {
		return fail(s.env, err)
	}
	fmt.Fprintln(s.env.Stdout)
	switch len(reg.Accounts) {
	case 0:
		fmt.Fprintln(s.env.Stdout, "Next: sign in to Claude, then run setup again")
		fmt.Fprintln(s.env.Stdout, "  claude auth login")
	case 1:
		fmt.Fprintln(s.env.Stdout, "Next: add your second account (opens a browser, once)")
		fmt.Fprintln(s.env.Stdout, "  claude-go accounts add personal --login")
	default:
		fmt.Fprintln(s.env.Stdout, "All set. Start Claude as usual:")
		fmt.Fprintln(s.env.Stdout, "  claude")
	}
	if !hookOK {
		return exitFailure
	}
	return exitOK
}

func (s *setup) adoptDefaultLogin(claudePath string, baseEnv []string) {
	ident, err := claudebin.WhoAmI(s.ctx, s.env.Runner, launcher.ProfileEnv(baseEnv, launcher.Account{Default: true}), claudePath)
	switch {
	case err != nil:
		s.row(symWarn, "Your login", "could not read it: %v", err)
		return
	case !ident.LoggedIn:
		s.row(symWarn, "Your login", "Claude is not signed in (run: claude auth login)")
		return
	}
	dir := ident.ConfigDir
	if dir == "" {
		dir = s.layout.DefaultClaudeDir
	}
	acct, adopted, err := s.service().AdoptDefault(defaultAccountID, registry.Account{Email: ident.Email, OrgID: ident.OrgID}, dir)
	switch {
	case errors.Is(err, accounts.ErrSameLogin):
		s.row(symWarn, "Your login", "already registered under another name: %v", err)
	case err != nil:
		s.row(symWarn, "Your login", "could not register it: %v", err)
	case adopted:
		s.row(symOK, "Your login", "%s", ident.Email)
	default:
		s.row(symOK, "Your login", "%s", firstNonEmptyStr(acct.Email, acct.ID))
	}
}

func (s *setup) installBinary() string {
	self, err := s.env.Executable()
	if err != nil {
		s.row(symWarn, "Command", "cannot locate this executable: %v", err)
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	target := filepath.Join(s.home, installRelDir, binaryName)
	if self == target {
		s.row(symOK, "Command", "%s", s.tilde(target))
		return target
	}
	if !s.install {
		s.row(symSkip, "Command", "running from %s (it must stay there for the hook to work)", s.tilde(self))
		return self
	}
	data, err := os.ReadFile(self)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(target), 0o755)
	}
	if err == nil {
		err = fsx.WriteFileAtomic(target, data, 0o755)
	}
	if err != nil {
		s.row(symWarn, "Command", "could not install to %s: %v (using %s)", s.tilde(target), err, s.tilde(self))
		return self
	}
	s.row(symOK, "Command", "%s", s.tilde(target))
	return target
}

func (s *setup) installHook(binary string) bool {
	if binary == "" {
		s.row(symFail, "Limit hook", "not installed: no claude-go executable path")
		return false
	}
	in := hookinstall.Installer{SettingsPath: filepath.Join(s.layout.DefaultClaudeDir, "settings.json"), Binary: binary}
	res, err := in.Install()
	switch {
	case err != nil:
		s.row(symFail, "Limit hook", "not installed: %v", err)
		return false
	case res.Changed && res.Backup != "":
		s.row(symOK, "Limit hook", "added to %s (backup kept)", s.tilde(in.SettingsPath))
	case res.Changed:
		s.row(symOK, "Limit hook", "added to %s", s.tilde(in.SettingsPath))
	default:
		s.row(symOK, "Limit hook", "already in %s", s.tilde(in.SettingsPath))
	}
	return true
}

// repairProfiles makes existing profiles pick up the shared settings (and so the
// hook), and fills in who each account is so duplicate logins can be spotted.
func (s *setup) repairProfiles(claudePath string, baseEnv []string) {
	reg, err := registry.Load(s.layout.Accounts)
	if err != nil {
		s.row(symWarn, "Accounts", "%v", err)
		return
	}
	profiles := s.service().Profiles()
	for _, acct := range reg.Accounts {
		if acct.Default {
			continue
		}
		if changed, err := profiles.SeedState(acct.ID); err != nil {
			s.row(symWarn, "Profile "+acct.ID, "%v", err)
		} else if changed {
			s.row(symOK, "Profile "+acct.ID, "your onboarding and trusted folders carried over (no first-run screens)")
		}
		if err := profiles.LinkSharedProjects(acct.ID); err != nil {
			s.row(symWarn, "Profile "+acct.ID, "%v", err)
		}
		_, skipped, err := profiles.LinkUserConfig(acct.ID)
		if err != nil {
			s.row(symWarn, "Profile "+acct.ID, "%v", err)
		}
		for _, name := range skipped {
			if name == "settings.json" {
				s.row(symWarn, "Profile "+acct.ID, "has its own settings.json, so the limit hook will not run there; delete it to share yours (profile %q)", acct.ID)
			}
		}
		if acct.Email == "" {
			s.backfillIdentity(claudePath, baseEnv, acct)
		}
	}
	reg, err = registry.Load(s.layout.Accounts)
	if err != nil {
		return
	}
	for i, a := range reg.Accounts {
		for _, b := range reg.Accounts[i+1:] {
			if registry.SameLogin(a, b) {
				s.row(symWarn, "Accounts", "%q and %q are the same login (%s), so switching between them cannot help. Remove one: claude-go accounts remove %s --purge-profile", a.ID, b.ID, a.Email, b.ID)
			}
		}
	}
}

func (s *setup) backfillIdentity(claudePath string, baseEnv []string, acct registry.Account) {
	la, err := s.launchAccount(acct)
	if err != nil {
		return
	}
	ident, err := claudebin.WhoAmI(s.ctx, s.env.Runner, launcher.ProfileEnv(baseEnv, la), claudePath)
	if err != nil || !ident.LoggedIn {
		return
	}
	_ = registry.Mutate(s.layout.Accounts, func(f *registry.File) error {
		for i := range f.Accounts {
			if f.Accounts[i].ID == acct.ID {
				f.Accounts[i].Email, f.Accounts[i].OrgID = ident.Email, ident.OrgID
			}
		}
		return nil
	})
}

func (s *setup) shellBlock(binary string) {
	rc, ok := shellrc.RCFile(s.env.Getenv("SHELL"), s.home)
	var lines []string
	installDir := filepath.Join(s.home, installRelDir)
	if filepath.Dir(binary) == installDir && !pathContains(s.env.Getenv("PATH"), installDir) {
		lines = append(lines, `export PATH="$HOME/`+installRelDir+`:$PATH"`)
	}
	alias := ""
	if binary != "" && !strings.ContainsAny(binary, "'\"\n") {
		alias = fmt.Sprintf("alias claude='\"%s\" run'", binary)
	}
	if s.alias && alias != "" {
		lines = append(lines, alias)
	}
	if len(lines) == 0 {
		if alias != "" && !s.alias {
			s.row(symSkip, "Shortcut", "not added (`claude` still starts plain Claude)")
		}
		return
	}
	if !ok {
		s.row(symSkip, "Shortcut", "your shell (%s) is not edited automatically; add to its startup file:", s.env.Getenv("SHELL"))
		for _, l := range lines {
			fmt.Fprintf(s.env.Stdout, "      %s\n", l)
		}
		return
	}
	changed, err := shellrc.Apply(rc, lines)
	switch {
	case err != nil:
		s.row(symWarn, "Shortcut", "could not update %s: %v", s.tilde(rc), err)
	case changed:
		s.row(symOK, "Shortcut", "`claude` now starts claude-go (open a new terminal)")
	default:
		s.row(symOK, "Shortcut", "already set in %s", s.tilde(rc))
	}
}

func pathContains(pathVar, dir string) bool {
	for _, p := range filepath.SplitList(pathVar) {
		if filepath.Clean(p) == dir {
			return true
		}
	}
	return false
}

func (s *setup) installStatusLine(binary string) {
	in := hookinstall.Installer{SettingsPath: filepath.Join(s.layout.DefaultClaudeDir, "settings.json"), Binary: binary}
	_, mode, err := in.InstallStatusLine()
	switch {
	case err != nil:
		s.row(symWarn, "Status line", "not changed: %v", err)
	case mode == hookinstall.StatusLineWrapped:
		s.row(symOK, "Status line", "added after your existing one")
	case mode == hookinstall.StatusLineUnchanged:
		s.row(symOK, "Status line", "already set up")
	default:
		s.row(symOK, "Status line", "added")
	}
}

func (s *setup) undo() int {
	fmt.Fprintln(s.env.Stdout, "claude-go setup --undo")
	fmt.Fprintln(s.env.Stdout)
	status := exitOK
	bin, _ := s.env.Executable()
	in := hookinstall.Installer{SettingsPath: filepath.Join(s.layout.DefaultClaudeDir, "settings.json"), Binary: bin}
	if res, err := in.Uninstall(); err != nil {
		s.row(symFail, "Limit hook", "not removed: %v", err)
		status = exitFailure
	} else if res.Changed {
		s.row(symOK, "Limit hook", "removed from %s", s.tilde(in.SettingsPath))
	} else {
		s.row(symSkip, "Limit hook", "was not installed")
	}
	if res, err := in.UninstallStatusLine(); err != nil {
		s.row(symFail, "Status line", "not restored: %v", err)
		status = exitFailure
	} else if res.Changed {
		s.row(symOK, "Status line", "restored")
	}
	if removed, err := s.disableMemory(); err != nil {
		s.row(symFail, "Memory", "not turned off: %v", err)
		status = exitFailure
	} else if removed {
		dir, _ := s.memoryDir()
		s.row(symOK, "Memory", "turned off (your notes are kept in %s)", s.tilde(dir))
	}
	if s.uninstallMod() != nil {
		status = exitFailure
	}
	if rc, ok := shellrc.RCFile(s.env.Getenv("SHELL"), s.home); ok {
		if changed, err := shellrc.Remove(rc); err != nil {
			s.row(symFail, "Shortcut", "%s not changed: %v", s.tilde(rc), err)
			status = exitFailure
		} else if changed {
			s.row(symOK, "Shortcut", "removed from %s", s.tilde(rc))
		}
	}
	fmt.Fprintln(s.env.Stdout)
	fmt.Fprintln(s.env.Stdout, "Your accounts and the claude-go command were left in place.")
	return status
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
