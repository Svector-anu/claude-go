// Package hookinstall manages claude-go's entry in a Claude Code settings.json.
//
// The file is shared with the user and with other tools, so claude-go only
// ever touches hook commands it can recognise as its own by signature (the
// executable is named claude-go and its first argument is "hook"). Everything
// else, including unknown keys and other tools' hooks on the same event, is
// preserved.
package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Svector-anu/claude-go/internal/fsx"
)

const (
	// Event is the Claude Code hook event claude-go listens to.
	Event = "StopFailure"
	// Subcommand is the claude-go subcommand the hook runs.
	Subcommand = "stop-failure"

	signatureBinary = "claude-go"
	signatureArg    = "hook"
	hookTimeout     = 10
	backupSuffix    = ".claude-go-backup"
)

// Installer edits one settings file.
type Installer struct {
	// SettingsPath is the settings.json to edit (usually ~/.claude/settings.json).
	SettingsPath string
	// Binary is the absolute path the hook should run.
	Binary string
}

// Result reports what an operation changed.
type Result struct {
	Changed bool
	Backup  string // set when a backup of the original file was written
}

// Command is the exact hook command line for binary.
func Command(binary string) string {
	return shellQuote(binary) + " " + signatureArg + " " + Subcommand
}

// IsOurs reports whether command is a claude-go hook invocation.
func IsOurs(command string) bool {
	f := fields(command)
	return len(f) >= 2 && filepath.Base(f[0]) == signatureBinary && f[1] == signatureArg
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$`&|;<>()*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fields splits a command line into words, honouring single and double quotes.
func fields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	started, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, started = false, true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t':
			if started || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

type settings map[string]any

func (i Installer) load() (settings, []byte, error) {
	raw, err := os.ReadFile(i.SettingsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return settings{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", i.SettingsPath, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return settings{}, raw, nil
	}
	var s settings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		return nil, nil, fmt.Errorf("%s is not valid JSON; claude-go will not modify it: %w", i.SettingsPath, err)
	}
	if s == nil {
		return nil, nil, fmt.Errorf("%s is not a JSON object; claude-go will not modify it", i.SettingsPath)
	}
	return s, raw, nil
}

func (i Installer) save(s settings, original []byte) (Result, error) {
	target := i.SettingsPath
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	var res Result
	if original != nil {
		backup := target + backupSuffix
		if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
			if err := fsx.WriteFileAtomic(backup, original, 0o600); err != nil {
				return Result{}, fmt.Errorf("back up %s: %w", target, err)
			}
			res.Backup = backup
		}
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode settings: %w", err)
	}
	if err := fsx.WriteFileAtomic(target, append(data, '\n'), mode); err != nil {
		return Result{}, err
	}
	res.Changed = true
	return res, nil
}

// hookEntries returns every hook object under the event's matcher groups.
func eventGroups(s settings, event string) []any {
	hooks, _ := s["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	return groups
}

func hooksOf(group any) []any {
	g, _ := group.(map[string]any)
	hs, _ := g["hooks"].([]any)
	return hs
}

func commandOf(h any) (string, bool) {
	m, _ := h.(map[string]any)
	c, ok := m["command"].(string)
	return c, ok
}

// Install adds or refreshes claude-go's hook. It is idempotent: when the file
// already holds exactly the desired entry, nothing is written.
func (i Installer) Install() (Result, error) {
	s, original, err := i.load()
	if err != nil {
		return Result{}, err
	}
	want := Command(i.Binary)
	changed := false
	found := false

	hooks, _ := s["hooks"].(map[string]any)
	if hooks == nil {
		if _, exists := s["hooks"]; exists {
			return Result{}, fmt.Errorf("%s: \"hooks\" is not an object; claude-go will not modify it", i.SettingsPath)
		}
		hooks = map[string]any{}
	}
	groups, _ := hooks[Event].([]any)
	for gi, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hs, _ := gm["hooks"].([]any)
		kept := hs[:0:0]
		for _, h := range hs {
			cmd, ok := commandOf(h)
			if !ok || !IsOurs(cmd) {
				kept = append(kept, h)
				continue
			}
			if found { // duplicate of our own entry
				changed = true
				continue
			}
			found = true
			if cmd != want {
				h.(map[string]any)["command"] = want
				changed = true
			}
			kept = append(kept, h)
		}
		if len(kept) != len(hs) {
			gm["hooks"] = kept
		}
		groups[gi] = gm
	}
	groups = dropEmptyGroups(groups)
	if !found {
		groups = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": want, "timeout": json.Number(fmt.Sprint(hookTimeout))}},
		})
		changed = true
	}
	if !changed {
		return Result{}, nil
	}
	hooks[Event] = groups
	s["hooks"] = hooks
	return i.save(s, original)
}

func dropEmptyGroups(groups []any) []any {
	out := groups[:0:0]
	for _, g := range groups {
		if gm, ok := g.(map[string]any); ok {
			if hs, isList := gm["hooks"].([]any); isList && len(hs) == 0 {
				continue
			}
		}
		out = append(out, g)
	}
	return out
}

// Uninstall removes every claude-go hook from every event, leaving others alone.
func (i Installer) Uninstall() (Result, error) {
	s, original, err := i.load()
	if err != nil {
		return Result{}, err
	}
	hooks, _ := s["hooks"].(map[string]any)
	changed := false
	for event, raw := range hooks {
		groups, _ := raw.([]any)
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				continue
			}
			hs, _ := gm["hooks"].([]any)
			kept := hs[:0:0]
			for _, h := range hs {
				if cmd, ok := commandOf(h); ok && IsOurs(cmd) {
					changed = true
					continue
				}
				kept = append(kept, h)
			}
			gm["hooks"] = kept
		}
		groups = dropEmptyGroups(groups)
		if len(groups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = groups
		}
	}
	if !changed {
		return Result{}, nil
	}
	if len(hooks) == 0 {
		delete(s, "hooks")
	}
	return i.save(s, original)
}

// State describes the installed hook.
type State int

const (
	Absent State = iota
	Installed
	// Stale means a claude-go hook exists but runs a different command than expected.
	Stale
)

// Status inspects the settings file without modifying it.
func (i Installer) Status() (State, error) {
	s, _, err := i.load()
	if err != nil {
		return Absent, err
	}
	want := Command(i.Binary)
	state := Absent
	for _, g := range eventGroups(s, Event) {
		for _, h := range hooksOf(g) {
			if cmd, ok := commandOf(h); ok && IsOurs(cmd) {
				if cmd == want {
					return Installed, nil
				}
				state = Stale
			}
		}
	}
	return state, nil
}

// InstalledBinary returns the executable the installed claude-go hook runs.
func (i Installer) InstalledBinary() (binary string, found bool, err error) {
	s, _, err := i.load()
	if err != nil {
		return "", false, err
	}
	for _, g := range eventGroups(s, Event) {
		for _, h := range hooksOf(g) {
			if cmd, ok := commandOf(h); ok && IsOurs(cmd) {
				return fields(cmd)[0], true, nil
			}
		}
	}
	return "", false, nil
}

// --- status line -----------------------------------------------------------

const (
	statusLineKey        = "statusLine"
	statusLineSubcommand = "statusline"
	statusLineThenFlag   = "--then"
)

// StatusLineCommand is the statusLine command that renders claude-go's segment.
// A non-empty original is the user's own command, which keeps running first
// and whose output claude-go's segment is appended to.
func StatusLineCommand(binary, original string) string {
	cmd := shellQuote(binary) + " " + statusLineSubcommand
	if original != "" {
		cmd += " " + statusLineThenFlag + " " + shellQuote(original)
	}
	return cmd
}

// IsOurStatusLine reports whether command is a claude-go statusline invocation.
func IsOurStatusLine(command string) bool {
	f := fields(command)
	return len(f) >= 2 && filepath.Base(f[0]) == signatureBinary && f[1] == statusLineSubcommand
}

// WrappedStatusLine returns the user's original command inside a claude-go one.
func WrappedStatusLine(command string) (original string, ok bool) {
	f := fields(command)
	if len(f) < 4 || !IsOurStatusLine(command) {
		return "", false
	}
	for i := 2; i+1 < len(f); i++ {
		if f[i] == statusLineThenFlag {
			return f[i+1], true
		}
	}
	return "", false
}

// StatusLineMode says what InstallStatusLine did.
type StatusLineMode string

const (
	StatusLineAdded     StatusLineMode = "added"     // there was no status line
	StatusLineWrapped   StatusLineMode = "wrapped"   // the user's command now runs first
	StatusLineUpdated   StatusLineMode = "updated"   // claude-go's entry pointed elsewhere
	StatusLineUnchanged StatusLineMode = "unchanged" // already correct
)

func (i Installer) statusLine(s settings) (map[string]any, bool, error) {
	raw, exists := s[statusLineKey]
	if !exists || raw == nil {
		return nil, false, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, true, fmt.Errorf("%s: \"statusLine\" is not an object; claude-go will not modify it", i.SettingsPath)
	}
	return m, true, nil
}

// InstallStatusLine adds claude-go's segment to the status line, wrapping a
// status line the user already has rather than replacing it.
func (i Installer) InstallStatusLine() (Result, StatusLineMode, error) {
	s, original, err := i.load()
	if err != nil {
		return Result{}, "", err
	}
	current, exists, err := i.statusLine(s)
	if err != nil {
		return Result{}, "", err
	}
	if !exists {
		s[statusLineKey] = map[string]any{"type": "command", "command": StatusLineCommand(i.Binary, "")}
		res, err := i.save(s, original)
		return res, StatusLineAdded, err
	}
	if t, _ := current["type"].(string); t != "command" {
		return Result{}, "", fmt.Errorf("%s: the existing statusLine is not a command; claude-go will not modify it", i.SettingsPath)
	}
	cmd, _ := current["command"].(string)
	if cmd == "" {
		return Result{}, "", fmt.Errorf("%s: the existing statusLine has no command; claude-go will not modify it", i.SettingsPath)
	}
	if IsOurStatusLine(cmd) {
		orig, _ := WrappedStatusLine(cmd)
		want := StatusLineCommand(i.Binary, orig)
		if cmd == want {
			return Result{}, StatusLineUnchanged, nil
		}
		current["command"] = want
		res, err := i.save(s, original)
		return res, StatusLineUpdated, err
	}
	current["command"] = StatusLineCommand(i.Binary, cmd)
	res, err := i.save(s, original)
	return res, StatusLineWrapped, err
}

// UninstallStatusLine restores the user's own status line, or removes the one
// claude-go added. A status line that is not claude-go's is never touched.
func (i Installer) UninstallStatusLine() (Result, error) {
	s, original, err := i.load()
	if err != nil {
		return Result{}, err
	}
	current, exists, err := i.statusLine(s)
	if err != nil || !exists {
		return Result{}, err
	}
	cmd, _ := current["command"].(string)
	if !IsOurStatusLine(cmd) {
		return Result{}, nil
	}
	if orig, ok := WrappedStatusLine(cmd); ok {
		current["command"] = orig
	} else {
		delete(s, statusLineKey)
	}
	return i.save(s, original)
}

// StatusLineInstalled reports whether claude-go's segment is in the status line.
func (i Installer) StatusLineInstalled() (bool, error) {
	s, _, err := i.load()
	if err != nil {
		return false, err
	}
	current, exists, err := i.statusLine(s)
	if err != nil || !exists {
		return false, err
	}
	cmd, _ := current["command"].(string)
	return IsOurStatusLine(cmd), nil
}
