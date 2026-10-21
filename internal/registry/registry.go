// Package registry holds the list of accounts claude-go manages.
//
// The registry stores identity metadata only. Credentials are owned by Claude Code
// inside each account's profile directory and are never read or stored here.
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"claude-go/internal/fsx"
)

const (
	currentVersion = 1
	lockTimeout    = 5 * time.Second

	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ErrInvalidID is returned for IDs that are not safe to use as a directory name.
var ErrInvalidID = errors.New("invalid account id (use 1-32 chars of a-z, 0-9, '-' or '_', starting with a letter or digit)")

// ValidateID reports whether id is a safe account identifier. IDs become
// directory names, so path separators and dot segments are rejected.
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// Account is one user-owned Claude Code account.
type Account struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Email string `json:"email,omitempty"`
	OrgID string `json:"org_id,omitempty"`
	// Default marks the login Claude Code uses when CLAUDE_CONFIG_DIR is unset.
	// It is launched without exporting CLAUDE_CONFIG_DIR, because Claude derives
	// a different Keychain item name from an explicit value. ConfigDir records
	// where that default lives (normally ~/.claude).
	Default   bool      `json:"default,omitempty"`
	ConfigDir string    `json:"config_dir,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SameLogin reports whether two accounts identify the same Claude login.
// Accounts without a known email and org never match.
func SameLogin(a, b Account) bool {
	return a.Email != "" && a.Email == b.Email && a.OrgID == b.OrgID
}

// File is the on-disk registry document.
type File struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Load reads path. A missing file is an empty registry.
// Corrupt, unknown-field, or unsupported-version files are errors that name the path.
func Load(path string) (File, error) {
	empty := File{Version: currentVersion}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read registry %s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("parse registry %s: %w", path, err)
	}
	if f.Version != currentVersion {
		return File{}, fmt.Errorf("registry %s has unsupported version %d (this build supports %d)", path, f.Version, currentVersion)
	}
	seen := make(map[string]bool, len(f.Accounts))
	defaults := 0
	for i, a := range f.Accounts {
		if a.Default {
			defaults++
			if !filepath.IsAbs(a.ConfigDir) {
				return File{}, fmt.Errorf("registry %s: default account %q needs an absolute config_dir", path, a.ID)
			}
		}
		if err := ValidateID(a.ID); err != nil {
			return File{}, fmt.Errorf("registry %s: account %d: %w", path, i, err)
		}
		if seen[a.ID] {
			return File{}, fmt.Errorf("registry %s: duplicate account id %q", path, a.ID)
		}
		seen[a.ID] = true
	}
	if defaults > 1 {
		return File{}, fmt.Errorf("registry %s: %d accounts are marked default (at most one allowed)", path, defaults)
	}
	return f, nil
}

// Save atomically writes f to path with mode 0600, creating the parent
// directory with mode 0700 if needed.
func Save(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	f.Version = currentVersion
	if f.Accounts == nil {
		f.Accounts = []Account{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	return fsx.WriteFileAtomic(path, append(data, '\n'), fileMode)
}

// Mutate loads the registry under an exclusive lock, applies fn, and saves the
// result. If fn returns an error nothing is written. A corrupt registry aborts
// before fn runs, so corrupt state is never overwritten.
func Mutate(path string, fn func(*File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	lock, err := fsx.AcquireLock(path+".lock", lockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	f, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	return Save(path, f)
}
