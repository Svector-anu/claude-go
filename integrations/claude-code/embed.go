// Package claudecode embeds the optional Claude Code mod so the claude-go
// binary can install it without any network or build step.
package claudecode

import (
	"embed"
	"io/fs"
)

//go:embed all:plugin
var files embed.FS

// Plugin returns the mod's files rooted at the plugin folder.
func Plugin() (fs.FS, error) { return fs.Sub(files, "plugin") }
