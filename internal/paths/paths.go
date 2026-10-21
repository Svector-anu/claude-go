// Package paths resolves where claude-go keeps its own state.
package paths

import (
	"errors"
	"fmt"
	"path/filepath"
)

const (
	// EnvHome overrides the state directory.
	EnvHome = "CLAUDE_GO_HOME"

	defaultDirName = ".claude-go"
)

// Layout is the set of locations claude-go reads and writes.
type Layout struct {
	Root     string
	Config   string
	Accounts string
	Runtime  string
	// State holds account availability (limits, cooldowns).
	State string
	// History is the append-only log of handoff outcomes.
	History string
	// DefaultClaudeDir is Claude Code's own config directory (~/.claude). It is
	// empty when the home directory could not be determined.
	DefaultClaudeDir string
	// DefaultClaudeJSON is the default account's state file (~/.claude.json),
	// a sibling of ~/.claude rather than a file inside it.
	DefaultClaudeJSON string
}

// Resolve returns the layout, honouring CLAUDE_GO_HOME before the user's home directory.
func Resolve(getenv func(string) string, userHome func() (string, error)) (Layout, error) {
	home, homeErr := userHome()
	root := getenv(EnvHome)
	if root == "" {
		if homeErr != nil {
			return Layout{}, fmt.Errorf("resolve home directory (set %s to override): %w", EnvHome, homeErr)
		}
		if home == "" {
			return Layout{}, errors.New("home directory is empty (set " + EnvHome + " to override)")
		}
		root = filepath.Join(home, defaultDirName)
	}
	var claudeDir, claudeJSON string
	if homeErr == nil && home != "" {
		claudeDir = filepath.Join(home, ".claude")
		claudeJSON = filepath.Join(home, ".claude.json")
	}
	if !filepath.IsAbs(root) {
		return Layout{}, fmt.Errorf("%s must be an absolute path, got %q", EnvHome, root)
	}
	return Layout{
		Root:              root,
		Config:            filepath.Join(root, "config.json"),
		Accounts:          filepath.Join(root, "accounts.json"),
		Runtime:           filepath.Join(root, "runtime"),
		State:             filepath.Join(root, "state.json"),
		History:           filepath.Join(root, "handoffs.jsonl"),
		DefaultClaudeDir:  claudeDir,
		DefaultClaudeJSON: claudeJSON,
	}, nil
}
