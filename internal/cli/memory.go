package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/Svector-anu/claude-go/internal/buildinfo"
	"github.com/Svector-anu/claude-go/internal/config"
	"github.com/Svector-anu/claude-go/internal/mcp"
	"github.com/Svector-anu/claude-go/internal/memory"
	"github.com/Svector-anu/claude-go/internal/memorymcp"
)

const memoryUsage = `Usage:
  claude-go memory list [--all] [--project DIR]    saved notes (this project and global by default)
  claude-go memory forget [--global] [--project DIR] KEY
  claude-go memory export FILE                     bundle every note, to move or back up
  claude-go memory import FILE                     merge a bundle (safe to repeat)
  claude-go memory path                            where the notes are stored
  claude-go memory encrypt                         encrypt the notes on this machine (key kept in the keychain)
  claude-go memory decrypt                         write them as plain text again
  claude-go memory key export FILE | import FILE   move the key to another machine (a secret: delete the file after)
`

// memoryDir is where notes live: memory_dir from config, else under the state directory.
func (a *app) memoryDir() (string, error) {
	cfg, _, err := config.Load(a.layout.Config)
	if err != nil {
		return "", err
	}
	if cfg.MemoryDir == "" {
		return filepath.Join(a.layout.Root, "memory"), nil
	}
	if !filepath.IsAbs(cfg.MemoryDir) {
		return "", fmt.Errorf("memory_dir in %s must be an absolute path, got %q", a.layout.Config, cfg.MemoryDir)
	}
	return cfg.MemoryDir, nil
}

func (a *app) memoryStore() (*memory.Store, error) {
	dir, err := a.memoryDir()
	if err != nil {
		return nil, err
	}
	s, err := memory.Open(dir)
	if err != nil {
		return nil, err
	}
	s.Now = a.env.Now
	key, err := a.keyStore().Load()
	switch {
	case errors.Is(err, memory.ErrNoKey):
	case err != nil:
		// Writing plain text because the key was unreadable would defeat encryption.
		return nil, fmt.Errorf("memory is encrypted but its key cannot be read: %w", err)
	default:
		if s.Cipher, err = memory.NewCipher(key); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// keyStore is where the memory key lives.
func (a *app) keyStore() memory.KeyStore {
	if a.env.Keys != nil {
		return a.env.Keys
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/security"); err == nil {
			return memory.NewKeychainKeyStore("claude-go-memory", "key")
		}
	}
	return memory.FileKeyStore{Path: filepath.Join(a.layout.Root, "memory.key")}
}

// warnLocked tells the person when notes exist that this machine cannot read.
func (a *app) warnLocked(store *memory.Store) {
	if n, err := store.Locked(); err == nil && n > 0 {
		fmt.Fprintf(a.env.Stderr, "%d encrypted note line(s) cannot be read here: this machine has no matching key. Run `claude-go memory key import FILE` with a key exported from the machine that wrote them.\n", n)
	}
}

// runMCPMemory serves the memory tools over stdio. Stdout carries protocol
// frames only; anything else would corrupt the stream, so problems go to stderr.
func runMCPMemory(ctx context.Context, env Env) int {
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	store, err := a.memoryStore()
	if err != nil {
		return fail(env, err)
	}
	cwd, _ := env.Getwd() // an unknown directory just means global-only notes
	var srv *mcp.Server = memorymcp.NewServer(store, cwd, buildinfo.Version)
	if err := srv.Serve(ctx, env.Stdin, env.Stdout); err != nil {
		return fail(env, err)
	}
	return exitOK
}

func runMemory(args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, memoryUsage)
		return exitUsage
	}
	a, err := newApp(env)
	if err != nil {
		return fail(env, err)
	}
	switch args[0] {
	case "path":
		dir, err := a.memoryDir()
		if err != nil {
			return fail(env, err)
		}
		fmt.Fprintln(env.Stdout, dir)
		return exitOK
	case "list":
		return a.memoryList(args[1:])
	case "forget":
		return a.memoryForget(args[1:])
	case "export":
		return a.memoryExport(args[1:])
	case "import":
		return a.memoryImport(args[1:])
	case "encrypt":
		return a.memoryEncrypt()
	case "decrypt":
		return a.memoryDecrypt()
	case "key":
		return a.memoryKey(args[1:])
	default:
		fmt.Fprintf(env.Stderr, "claude-go memory: unknown subcommand %q\n\n%s", args[0], memoryUsage)
		return exitUsage
	}
}

func (a *app) projectFor(dir string) (scope, name string, err error) {
	if dir == "" {
		dir, err = a.env.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("determine working directory: %w", err)
		}
	}
	scope, name = memory.ProjectScope(dir)
	return scope, name, nil
}

func (a *app) memoryList(args []string) int {
	fs := newFlagSet("memory list", a.env)
	all := fs.Bool("all", false, "show every project's notes")
	project := fs.String("project", "", "directory of the project (default: the current one)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	var scopes []string
	if !*all {
		scope, _, err := a.projectFor(*project)
		if err != nil {
			return fail(a.env, err)
		}
		scopes = []string{memory.GlobalScope}
		if scope != "" {
			scopes = append(scopes, scope)
		}
	}
	facts, err := store.Facts(scopes...)
	if err != nil {
		return fail(a.env, err)
	}
	defer a.warnLocked(store)
	if len(facts) == 0 {
		fmt.Fprintln(a.env.Stdout, "No notes saved yet.")
		return exitOK
	}
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tKEY\tNOTE\tUPDATED")
	for _, f := range facts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortScope(f.Scope), f.Key, clipText(f.Value, 70), humanSince(f.UpdatedAt, a.env.Now()))
	}
	if err := tw.Flush(); err != nil {
		return fail(a.env, err)
	}
	return exitOK
}

func shortScope(s string) string {
	if s == memory.GlobalScope {
		return "global"
	}
	return strings.TrimPrefix(s, "project:")
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func (a *app) memoryForget(args []string) int {
	fs := newFlagSet("memory forget", a.env)
	global := fs.Bool("global", false, "forget a global note instead of a project note")
	project := fs.String("project", "", "directory of the project (default: the current one)")
	key, code, ok := parseWithID(fs, args, a.env)
	if !ok {
		return code
	}
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	scope := memory.GlobalScope
	if !*global {
		scope, _, err = a.projectFor(*project)
		if err != nil {
			return fail(a.env, err)
		}
		if scope == "" {
			return fail(a.env, errors.New("no project here; use --global or --project DIR"))
		}
	}
	existed, err := store.Forget(scope, key)
	if err != nil {
		return fail(a.env, err)
	}
	if !existed {
		fmt.Fprintf(a.env.Stdout, "No note %q in %s.\n", key, shortScope(scope))
		return exitFailure
	}
	fmt.Fprintf(a.env.Stdout, "Forgot %q from %s.\n", key, shortScope(scope))
	return exitOK
}

func (a *app) memoryExport(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(a.env.Stderr, memoryUsage)
		return exitUsage
	}
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	f, err := os.OpenFile(args[0], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(a.env, fmt.Errorf("cannot create %s (it must not already exist): %w", args[0], err))
	}
	n, err := store.Export(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(a.env, err)
	}
	fmt.Fprintf(a.env.Stdout, "Exported %d entries to %s\n", n, args[0])
	if store.Cipher != nil {
		fmt.Fprintln(a.env.Stdout, "Note: the export is plain text, even though your notes are encrypted. Keep it private and delete it after use.")
	}
	return exitOK
}

func (a *app) memoryImport(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(a.env.Stderr, memoryUsage)
		return exitUsage
	}
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	f, err := os.Open(args[0])
	if err != nil {
		return fail(a.env, err)
	}
	defer f.Close()
	res, err := store.Import(f)
	if err != nil {
		return fail(a.env, err)
	}
	fmt.Fprintf(a.env.Stdout, "Imported %d new entries (%d already known, %d rejected).\n", res.Added, res.Skipped, res.Rejected)
	if res.Rejected > 0 {
		return exitFailure
	}
	return exitOK
}

func (a *app) memoryEncrypt() int {
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	ks := a.keyStore()
	if store.Cipher == nil {
		key, err := memory.GenerateKey()
		if err != nil {
			return fail(a.env, err)
		}
		if err := ks.Save(key); err != nil {
			return fail(a.env, err)
		}
		if store.Cipher, err = memory.NewCipher(key); err != nil {
			return fail(a.env, err)
		}
	}
	if err := store.Rewrite(store.Cipher); err != nil {
		return fail(a.env, err)
	}
	fmt.Fprintln(a.env.Stdout, "Your notes on this machine are encrypted. New notes are encrypted as they are saved.")
	fmt.Fprintf(a.env.Stdout, "The key is kept in %s, never in the notes folder.\n", a.keyHome())
	fmt.Fprintln(a.env.Stdout, "To read them on another machine: claude-go memory key export FILE, then claude-go memory key import FILE there.")
	return exitOK
}

func (a *app) keyHome() string {
	if _, ok := a.keyStore().(memory.FileKeyStore); ok {
		return a.keyStore().(memory.FileKeyStore).Path + " (readable only by you)"
	}
	return "the macOS login keychain"
}

func (a *app) memoryDecrypt() int {
	store, err := a.memoryStore()
	if err != nil {
		return fail(a.env, err)
	}
	if store.Cipher == nil {
		fmt.Fprintln(a.env.Stdout, "Notes are not encrypted on this machine; nothing to do.")
		return exitOK
	}
	if err := store.Rewrite(nil); err != nil {
		return fail(a.env, err)
	}
	if n, err := store.Locked(); err == nil && n > 0 {
		fmt.Fprintf(a.env.Stdout, "Notes written by this machine are plain text again. The key was kept: %d encrypted line(s) from other machines still need it.\n", n)
		return exitOK
	}
	if err := a.keyStore().Delete(); err != nil {
		return fail(a.env, err)
	}
	fmt.Fprintln(a.env.Stdout, "Notes are plain text again and the key was removed.")
	return exitOK
}

func (a *app) memoryKey(args []string) int {
	if len(args) != 2 || (args[0] != "export" && args[0] != "import") {
		fmt.Fprint(a.env.Stderr, memoryUsage)
		return exitUsage
	}
	ks := a.keyStore()
	if args[0] == "export" {
		key, err := ks.Load()
		if errors.Is(err, memory.ErrNoKey) {
			return fail(a.env, errors.New("notes are not encrypted on this machine, so there is no key to export"))
		}
		if err != nil {
			return fail(a.env, err)
		}
		f, err := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(a.env, fmt.Errorf("cannot create %s (it must not already exist): %w", args[1], err))
		}
		_, werr := fmt.Fprintln(f, hex.EncodeToString(key))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fail(a.env, werr)
		}
		fmt.Fprintf(a.env.Stdout, "Key written to %s. Anyone with this file can read your notes: move it to the other machine privately, then delete it.\n", args[1])
		return exitOK
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return fail(a.env, err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != memory.KeyLen {
		return fail(a.env, fmt.Errorf("%s does not hold a memory key", args[1]))
	}
	if cur, err := ks.Load(); err == nil && !bytes.Equal(cur, key) {
		return fail(a.env, errors.New("a different memory key is already set up here; run `claude-go memory decrypt` first"))
	}
	if err := ks.Save(key); err != nil {
		return fail(a.env, err)
	}
	fmt.Fprintln(a.env.Stdout, "Key imported. Notes encrypted with it can now be read here; new notes are encrypted too. Delete the key file now.")
	return exitOK
}
