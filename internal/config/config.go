// Package config loads claude-go's user configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const currentVersion = 1

// Config is the on-disk user configuration. A missing file yields defaults.
type Config struct {
	Version int `json:"version"`
	// ClaudePath pins the Claude Code executable; empty means search PATH.
	ClaudePath string `json:"claude_path,omitempty"`
	// ContinueAfterLimit controls whether, after switching accounts because a
	// usage limit cut a request short, claude-go sends a short message so Claude
	// carries on with that request. Default true.
	ContinueAfterLimit *bool `json:"continue_after_limit,omitempty"`
	// ContinueMessage replaces the default continuation message.
	ContinueMessage string `json:"continue_message,omitempty"`
	// MemoryDir relocates the project memory (default ~/.claude-go/memory),
	// for example into a folder synced between machines. Must be absolute.
	MemoryDir string `json:"memory_dir,omitempty"`
}

// DefaultContinueMessage is sent after an automatic switch so the interrupted
// request is picked up without the user retyping it.
const DefaultContinueMessage = "[claude-go] Continuing after an account switch: your previous request was cut off by a usage limit on the other account. Please carry on with it."

// ContinuePrompt returns the message to send after a limit-triggered switch, or
// "" when the user turned the behaviour off.
func (c Config) ContinuePrompt() string {
	if c.ContinueAfterLimit != nil && !*c.ContinueAfterLimit {
		return ""
	}
	if msg := strings.TrimSpace(c.ContinueMessage); msg != "" {
		return msg
	}
	return DefaultContinueMessage
}

// Load reads path. A missing file returns defaults and exists=false.
// Malformed, unknown-field, or unsupported-version files are errors that name the path.
func Load(path string) (cfg Config, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{Version: currentVersion}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, true, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Version != currentVersion {
		return Config{}, true, fmt.Errorf("config %s has unsupported version %d (this build supports %d)", path, cfg.Version, currentVersion)
	}
	return cfg, true, nil
}
