// Package transcript locates Claude Code session transcripts.
package transcript

import (
	"os"
	"path/filepath"
)

// Find returns the transcript for sessionID under configDir/projects. It
// searches every project directory instead of re-deriving Claude's path
// encoding of the working directory, which is not a stable interface.
func Find(configDir, sessionID string) (string, bool) {
	if configDir == "" || sessionID == "" {
		return "", false
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", false
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return m, true
		}
	}
	return "", false
}
