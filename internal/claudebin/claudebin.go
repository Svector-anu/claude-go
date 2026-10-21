// Package claudebin finds the real Claude Code executable and asks it for its version.
package claudebin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs a command and returns its stdout. It exists so tests never spawn processes.
// A nil env inherits the current process environment.
type Runner interface {
	Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
}

// ExecRunner is the production Runner.
type ExecRunner struct{}

// Output runs name with args and returns stdout.
func (ExecRunner) Output(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.Output()
}

// ErrNotFound means no Claude Code executable could be located.
var ErrNotFound = errors.New("claude executable not found")

// Locate returns the configured path if set, otherwise the first "claude" on PATH.
func Locate(configured string, lookPath func(string) (string, error)) (string, error) {
	if configured != "" {
		resolved, err := lookPath(configured)
		if err != nil {
			return "", fmt.Errorf("%w: configured claude_path %q: %v", ErrNotFound, configured, err)
		}
		return resolved, nil
	}
	resolved, err := lookPath("claude")
	if err != nil {
		return "", fmt.Errorf("%w on PATH", ErrNotFound)
	}
	return resolved, nil
}

// Version returns the trimmed output of `<path> --version`.
func Version(ctx context.Context, r Runner, path string) (string, error) {
	out, err := r.Output(ctx, nil, path, "--version")
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", path, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("%s --version produced no output", path)
	}
	return v, nil
}

// Identity is what `claude auth status` reveals about a login. It contains no credentials.
type Identity struct {
	LoggedIn  bool
	Email     string
	OrgID     string
	ConfigDir string
}

// WhoAmI asks Claude Code about the login for the environment in env (which
// selects the profile via CLAUDE_CONFIG_DIR). Only the loggedIn, email and
// orgId fields are read.
func WhoAmI(ctx context.Context, r Runner, env []string, path string) (Identity, error) {
	out, err := r.Output(ctx, env, path, "auth", "status")
	var status struct {
		LoggedIn  *bool  `json:"loggedIn"`
		Email     string `json:"email"`
		OrgID     string `json:"orgId"`
		ConfigDir string `json:"configDirectory"`
	}
	// Claude may exit non-zero when logged out while still printing the JSON.
	if jsonErr := json.Unmarshal(out, &status); jsonErr == nil && status.LoggedIn != nil {
		return Identity{LoggedIn: *status.LoggedIn, Email: status.Email, OrgID: status.OrgID, ConfigDir: status.ConfigDir}, nil
	}
	if err != nil {
		return Identity{}, fmt.Errorf("run %s auth status: %w", path, err)
	}
	return Identity{}, fmt.Errorf("%s auth status: unrecognized output", path)
}

// LoggedIn reports only whether the profile in env holds a login.
func LoggedIn(ctx context.Context, r Runner, env []string, path string) (bool, error) {
	id, err := WhoAmI(ctx, r, env, path)
	return id.LoggedIn, err
}
