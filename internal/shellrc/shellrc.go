// Package shellrc maintains one clearly-marked block in a shell startup file.
package shellrc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Svector-anu/claude-go/internal/fsx"
)

const (
	beginMarker  = "# >>> claude-go >>>"
	endMarker    = "# <<< claude-go <<<"
	backupSuffix = ".claude-go-backup"
)

// RCFile picks the startup file for shell (the value of $SHELL) under home.
// ok is false for shells claude-go does not edit.
func RCFile(shell, home string) (path string, ok bool) {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(home, ".zshrc"), true
	case "bash":
		if _, err := os.Stat(filepath.Join(home, ".bash_profile")); err == nil {
			return filepath.Join(home, ".bash_profile"), true
		}
		return filepath.Join(home, ".bashrc"), true
	}
	return "", false
}

func read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// split returns the text before and after the managed block. found is false
// when there is no block; a begin marker without an end marker is an error so
// a damaged file is never guessed at.
func split(content string) (before, after string, found bool, err error) {
	b := strings.Index(content, beginMarker)
	if b < 0 {
		return content, "", false, nil
	}
	e := strings.Index(content[b:], endMarker)
	if e < 0 {
		return "", "", false, errors.New("found the claude-go begin marker without an end marker; fix the file by hand")
	}
	end := b + e + len(endMarker)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:b], content[end:], true, nil
}

func write(path, content, original string, exists bool) error {
	if exists {
		backup := path + backupSuffix
		if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
			if err := fsx.WriteFileAtomic(backup, []byte(original), 0o600); err != nil {
				return fmt.Errorf("back up %s: %w", path, err)
			}
		}
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	return fsx.WriteFileAtomic(target, []byte(content), mode)
}

// Apply installs or replaces the block with lines. It reports whether the file changed.
func Apply(path string, lines []string) (bool, error) {
	original, err := read(path)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(path)
	exists := statErr == nil
	before, after, found, err := split(original)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	block := beginMarker + "\n" + strings.Join(lines, "\n") + "\n" + endMarker + "\n"
	var updated string
	if found {
		updated = before + block + after
	} else {
		sep := ""
		if original != "" && !strings.HasSuffix(original, "\n") {
			sep = "\n"
		}
		updated = original + sep + block
	}
	if updated == original {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return true, write(path, updated, original, exists)
}

// Remove deletes the block, leaving the rest of the file untouched.
func Remove(path string) (bool, error) {
	original, err := read(path)
	if err != nil {
		return false, err
	}
	before, after, found, err := split(original)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !found {
		return false, nil
	}
	return true, write(path, before+after, original, true)
}
