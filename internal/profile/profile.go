// Package profile manages the per-account directories used as CLAUDE_CONFIG_DIR.
//
// Claude Code owns everything inside a profile, including credentials. This
// package only creates, locates and (on explicit request) deletes the directory.
package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"claude-go/internal/claudejson"
	"claude-go/internal/registry"
)

const dirMode os.FileMode = 0o700

// Manager owns <root>/profiles.
type Manager struct {
	Root string
	// DefaultDir is Claude Code's own config directory (~/.claude). When set,
	// its projects/ directory is the shared transcript store, so history from the
	// default account and every profile lives in one place.
	DefaultDir string
	// DefaultClaudeJSON is the default account's state file (~/.claude.json).
	// When set, the user's finished onboarding and trusted folders are carried
	// into each profile so a handoff never lands on a first-run wizard.
	DefaultClaudeJSON string
}

// Path returns the profile directory for id.
func (m Manager) Path(id string) (string, error) {
	if err := registry.ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(m.Root, "profiles", id), nil
}

// Ensure creates the profile directory (0700) if absent. created reports
// whether it was newly made; an existing directory is reused untouched.
func (m Manager) Ensure(id string) (path string, created bool, err error) {
	path, err = m.Path(id)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return "", false, fmt.Errorf("create profiles dir: %w", err)
	}
	fi, err := os.Lstat(path)
	switch {
	case err == nil && fi.IsDir():
		return path, false, nil
	case err == nil:
		return "", false, fmt.Errorf("%s exists and is not a directory", path)
	case !errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if err := os.Mkdir(path, dirMode); err != nil {
		return "", false, fmt.Errorf("create profile %s: %w", path, err)
	}
	return path, true, nil
}

// Remove deletes the profile directory and everything in it. It refuses
// symlinks so it can never delete data outside the profiles tree.
func (m Manager) Remove(id string) error {
	path, err := m.Path(id)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to remove %s: not a real directory", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove profile %s: %w", path, err)
	}
	return nil
}

// SharedProjects is the transcript store every profile links to, so a session
// started under one account can be resumed under another.
func (m Manager) SharedProjects() string {
	if m.DefaultDir != "" {
		return filepath.Join(m.DefaultDir, "projects")
	}
	return filepath.Join(m.Root, "shared", "projects")
}

// sharedUserConfig are the user's own settings, linked from the default config
// directory so every profile behaves like the default one (and so hooks
// installed once in the default settings.json apply everywhere).
var sharedUserConfig = []string{"settings.json", "keybindings.json", "CLAUDE.md", "commands", "agents", "skills", "rules"}

// LinkUserConfig symlinks the user's settings, CLAUDE.md, commands, agents,
// skills and rules from DefaultDir into the profile. Existing files in the
// profile are never replaced; they are returned in skipped so the caller can
// warn. Sources that do not exist are ignored.
func (m Manager) LinkUserConfig(id string) (linked, skipped []string, err error) {
	if m.DefaultDir == "" {
		return nil, nil, errors.New("default Claude config directory is unknown")
	}
	dir, err := m.Path(id)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range sharedUserConfig {
		src := filepath.Join(m.DefaultDir, name)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, name)
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				if target, rerr := os.Readlink(dst); rerr == nil && target == src {
					continue
				}
			}
			skipped = append(skipped, name)
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return linked, skipped, fmt.Errorf("inspect %s: %w", dst, err)
		}
		if err := symlink(src, dst); err != nil {
			return linked, skipped, err
		}
		linked = append(linked, name)
	}
	return linked, skipped, nil
}

// LinkSharedProjects points <profile>/projects at the shared transcript store.
// It never merges or deletes existing conversation history: a profile that
// already holds its own transcripts is an error the user must resolve.
func (m Manager) LinkSharedProjects(id string) error {
	profileDir, err := m.Path(id)
	if err != nil {
		return err
	}
	shared := m.SharedProjects()
	if err := os.MkdirAll(shared, dirMode); err != nil {
		return fmt.Errorf("create shared projects dir: %w", err)
	}
	link := filepath.Join(profileDir, "projects")
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return symlink(shared, link)
	case err != nil:
		return fmt.Errorf("inspect %s: %w", link, err)
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(link)
		if err != nil {
			return fmt.Errorf("read link %s: %w", link, err)
		}
		if target != shared {
			return fmt.Errorf("%s already links to %s, not the shared store %s", link, target, shared)
		}
		return nil
	case fi.IsDir():
		entries, err := os.ReadDir(link)
		if err != nil {
			return fmt.Errorf("read %s: %w", link, err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("%s already holds conversation history; claude-go will not merge or replace it", link)
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("replace empty %s: %w", link, err)
		}
		return symlink(shared, link)
	default:
		return fmt.Errorf("%s exists and is not a directory", link)
	}
}

func symlink(target, link string) error {
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("link %s -> %s: %w", link, target, err)
	}
	return nil
}

// Provision makes the profile ready to launch Claude Code: the directory
// exists, transcripts are shared, and (when DefaultDir is known) the user's
// settings are linked. warnings lists user-config files the profile already
// had of its own and that were therefore left alone.
func (m Manager) Provision(id string) (dir string, created bool, warnings []string, err error) {
	dir, created, err = m.Ensure(id)
	if err != nil {
		return "", false, nil, err
	}
	if err := m.LinkSharedProjects(id); err != nil {
		return dir, created, nil, err
	}
	if _, err := m.SeedState(id); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not carry your onboarding and trusted folders into this profile (Claude may ask again on first start): %v", err))
	}
	if m.DefaultDir == "" {
		return dir, created, warnings, nil
	}
	_, skipped, err := m.LinkUserConfig(id)
	if err != nil {
		return dir, created, nil, err
	}
	for _, name := range skipped {
		warnings = append(warnings, fmt.Sprintf("%s already exists in the profile and was not linked to your default one", name))
	}
	return dir, created, warnings, nil
}

// SeedState carries the user's finished onboarding and trusted folders into the
// profile's state file (additively; see package claudejson). It does nothing
// when the default state file is unknown.
func (m Manager) SeedState(id string) (changed bool, err error) {
	if m.DefaultClaudeJSON == "" {
		return false, nil
	}
	dir, err := m.Path(id)
	if err != nil {
		return false, err
	}
	state := filepath.Join(dir, ".claude.json")
	seeded, err := claudejson.Seed(m.DefaultClaudeJSON, state)
	if err != nil {
		return seeded, err
	}
	// The folder that holds the default config directory (the home folder) reads
	// that directory as project settings in a profile; those are the user's own.
	if filepath.Base(m.DefaultDir) != ".claude" {
		return seeded, nil
	}
	trusted, err := claudejson.TrustFolder(state, filepath.Dir(m.DefaultDir))
	return seeded || trusted, err
}
