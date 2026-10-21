// Package accounts ties the registry to profile directories.
package accounts

import (
	"errors"
	"fmt"
	"time"

	"github.com/Svector-anu/claude-go/internal/paths"
	"github.com/Svector-anu/claude-go/internal/profile"
	"github.com/Svector-anu/claude-go/internal/registry"
)

var (
	ErrExists           = errors.New("account already registered")
	ErrNotFound         = errors.New("account not registered")
	ErrSameLogin        = errors.New("that login is already registered")
	ErrDefaultProtected = errors.New("the default account lives in your own Claude config directory; claude-go never deletes it")
)

// Service performs account lifecycle operations.
type Service struct {
	Layout paths.Layout
	Now    func() time.Time
}

// Profiles returns the profile manager for this layout.
func (s Service) Profiles() profile.Manager {
	return profile.Manager{Root: s.Layout.Root, DefaultDir: s.Layout.DefaultClaudeDir, DefaultClaudeJSON: s.Layout.DefaultClaudeJSON}
}

// ConfigDir is the directory holding the account's Claude Code state.
func (s Service) ConfigDir(a registry.Account) (string, error) {
	if a.Default {
		return a.ConfigDir, nil
	}
	return s.Profiles().Path(a.ID)
}

// AddResult describes what Add did.
type AddResult struct {
	ProfileDir    string
	ReusedProfile bool
	Warnings      []string
}

// Add registers an account and provisions its profile directory. An existing
// profile directory (for example left by an earlier unregister) is adopted,
// never overwritten, and reported via ReusedProfile.
func (s Service) Add(a registry.Account) (AddResult, error) {
	return s.add(a, nil)
}

// LoginFunc signs the profile at dir in and reports who it is (Email, OrgID,
// and optionally a Label to use when the caller gave none). It must not return
// an identity for a login that did not complete.
type LoginFunc func(dir string) (registry.Account, error)

// AddWithLogin provisions the profile, runs login against it, and registers
// the account only once a login exists. A login that is the same as an
// already-registered one is rejected with ErrSameLogin, and a profile created
// by this call is removed on any failure.
func (s Service) AddWithLogin(a registry.Account, login LoginFunc) (AddResult, error) {
	return s.add(a, login)
}

func (s Service) add(a registry.Account, login LoginFunc) (AddResult, error) {
	if err := registry.ValidateID(a.ID); err != nil {
		return AddResult{}, err
	}
	labelGiven := a.Label != ""
	if !labelGiven {
		a.Label = a.ID
	}
	a.CreatedAt = s.Now().UTC()

	existing, err := registry.Load(s.Layout.Accounts)
	if err != nil {
		return AddResult{}, err
	}
	for _, e := range existing.Accounts {
		if e.ID == a.ID {
			return AddResult{}, fmt.Errorf("%w: %q", ErrExists, a.ID)
		}
	}

	dir, created, warnings, err := s.Profiles().Provision(a.ID)
	if err != nil {
		if created {
			_ = s.Profiles().Remove(a.ID)
		}
		return AddResult{}, err
	}
	rollback := func() {
		if created {
			_ = s.Profiles().Remove(a.ID)
		}
	}
	if login != nil {
		ident, err := login(dir)
		if err != nil {
			rollback()
			return AddResult{}, err
		}
		a.Email, a.OrgID = ident.Email, ident.OrgID
		if !labelGiven && ident.Label != "" {
			a.Label = ident.Label
		}
	}

	err = registry.Mutate(s.Layout.Accounts, func(f *registry.File) error {
		for _, e := range f.Accounts {
			if e.ID == a.ID {
				return fmt.Errorf("%w: %q", ErrExists, a.ID)
			}
			if registry.SameLogin(e, a) {
				return fmt.Errorf("%w: it is account %q (%s)", ErrSameLogin, e.ID, e.Email)
			}
		}
		f.Accounts = append(f.Accounts, a)
		return nil
	})
	if err != nil {
		rollback()
		return AddResult{}, err
	}
	return AddResult{ProfileDir: dir, ReusedProfile: !created, Warnings: warnings}, nil
}

// AdoptDefault registers the login Claude Code already uses (configDir,
// normally ~/.claude) without touching it. It is idempotent: if a default
// account exists, it is returned unchanged.
func (s Service) AdoptDefault(id string, ident registry.Account, configDir string) (registry.Account, bool, error) {
	if err := registry.ValidateID(id); err != nil {
		return registry.Account{}, false, err
	}
	var result registry.Account
	adopted := false
	err := registry.Mutate(s.Layout.Accounts, func(f *registry.File) error {
		for _, e := range f.Accounts {
			if e.Default {
				result = e
				return nil
			}
		}
		for _, e := range f.Accounts {
			if e.ID == id {
				return fmt.Errorf("%w: %q", ErrExists, id)
			}
			if registry.SameLogin(e, ident) {
				return fmt.Errorf("%w: it is account %q (%s)", ErrSameLogin, e.ID, e.Email)
			}
		}
		acct := registry.Account{
			ID: id, Label: ident.Email, Email: ident.Email, OrgID: ident.OrgID,
			Default: true, ConfigDir: configDir, CreatedAt: s.Now().UTC(),
		}
		if acct.Label == "" {
			acct.Label = id
		}
		// The default account goes first so it is the natural starting point.
		f.Accounts = append([]registry.Account{acct}, f.Accounts...)
		result, adopted = acct, true
		return nil
	})
	return result, adopted, err
}

// RemoveResult describes what Remove did.
type RemoveResult struct {
	ProfileDir string
	Purged     bool
}

// Remove unregisters an account. The profile directory, which may hold
// credentials, is kept unless purgeProfile is set. The default account's
// directory is never deleted.
func (s Service) Remove(id string, purgeProfile bool) (RemoveResult, error) {
	if err := registry.ValidateID(id); err != nil {
		return RemoveResult{}, err
	}
	dir, err := s.Profiles().Path(id)
	if err != nil {
		return RemoveResult{}, err
	}
	var wasDefault bool
	err = registry.Mutate(s.Layout.Accounts, func(f *registry.File) error {
		for i, a := range f.Accounts {
			if a.ID != id {
				continue
			}
			if a.Default && purgeProfile {
				return ErrDefaultProtected
			}
			wasDefault = a.Default
			f.Accounts = append(f.Accounts[:i:i], f.Accounts[i+1:]...)
			return nil
		}
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	})
	if err != nil {
		return RemoveResult{}, err
	}
	if wasDefault {
		return RemoveResult{ProfileDir: s.Layout.DefaultClaudeDir}, nil
	}
	res := RemoveResult{ProfileDir: dir}
	if purgeProfile {
		if err := s.Profiles().Remove(id); err != nil {
			return res, fmt.Errorf("account %q unregistered but profile was not deleted: %w", id, err)
		}
		res.Purged = true
	}
	return res, nil
}
