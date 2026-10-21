// Package availability remembers which accounts are out of rotation and picks
// the next one to use.
package availability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"claude-go/internal/fsx"
	"claude-go/internal/registry"
)

const (
	currentVersion = 1
	lockTimeout    = 5 * time.Second
)

// Status is why an account is out of rotation.
type Status string

const (
	// Exhausted: usage window spent until Until.
	Exhausted Status = "exhausted"
	// ModelLimited: one model is spent until Until; usable as a last resort.
	ModelLimited Status = "model_limited"
	// AuthFailed: needs `claude-go accounts login`; never expires on its own.
	AuthFailed Status = "auth_failed"
	// Blocked: billing or account hold; never expires on its own.
	Blocked Status = "blocked"
)

// Entry records one account's unavailability.
type Entry struct {
	Status    Status    `json:"status"`
	Until     time.Time `json:"until,omitzero"`
	Estimated bool      `json:"estimated,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Since     time.Time `json:"since"`
}

// blocking reports whether the entry still keeps the account out of rotation.
func (e Entry) blocking(now time.Time) bool {
	switch e.Status {
	case Exhausted, ModelLimited:
		return now.Before(e.Until)
	default:
		return true
	}
}

// Window is one rate-limit window as Claude last reported it for an account.
type Window struct {
	Kind        string    `json:"kind"`
	PercentUsed float64   `json:"percent_used"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// UsageSeen is the last reading of an account's windows. Claude only reports
// the account a session is running on, so this is always a reading of that
// moment (SeenAt), never a live value for other accounts.
type UsageSeen struct {
	SeenAt  time.Time `json:"seen_at"`
	Windows []Window  `json:"windows"`
}

// File is the persisted availability state.
type File struct {
	Version  int                  `json:"version"`
	Active   string               `json:"active,omitempty"`
	Accounts map[string]Entry     `json:"accounts,omitempty"`
	Usage    map[string]UsageSeen `json:"usage,omitempty"`
}

// exhaustedAt is the usage level at which a window is treated as spent.
const exhaustedAt = 95.0

// maxUsageAge bounds how old a reading may be and still date a reset.
const maxUsageAge = 6 * time.Hour

// RecordUsage stores the windows Claude reported for account id.
func RecordUsage(path, id string, u UsageSeen) error {
	return Mutate(path, func(f *File) {
		if f.Usage == nil {
			f.Usage = map[string]UsageSeen{}
		}
		f.Usage[id] = u
	})
}

// VerifiedReset returns when account id's spent windows reset, from Claude's
// own reported figures: the latest reset among windows at or above 95% with a
// reset still in the future, read within the last six hours.
func (f File) VerifiedReset(id string, now time.Time) (time.Time, bool) {
	u, ok := f.Usage[id]
	if !ok || now.Sub(u.SeenAt) > maxUsageAge {
		return time.Time{}, false
	}
	var at time.Time
	for _, w := range u.Windows {
		if w.PercentUsed >= exhaustedAt && w.ResetsAt.After(now) && w.ResetsAt.After(at) {
			at = w.ResetsAt
		}
	}
	return at, !at.IsZero()
}

// Load reads path. A missing file is empty state; corrupt state is an error.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{Version: currentVersion}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read state %s: %w", path, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if f.Version != currentVersion {
		return File{}, fmt.Errorf("state %s has unsupported version %d (this build supports %d)", path, f.Version, currentVersion)
	}
	return f, nil
}

// Mutate loads state under an exclusive lock, applies fn and saves atomically.
func Mutate(path string, fn func(*File)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	fn(&f)
	f.Version = currentVersion
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return fsx.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// Mark puts an account out of rotation.
func Mark(path, id string, e Entry) error {
	return Mutate(path, func(f *File) {
		if f.Accounts == nil {
			f.Accounts = map[string]Entry{}
		}
		f.Accounts[id] = e
	})
}

// Clear returns an account to rotation.
func Clear(path, id string) error {
	return Mutate(path, func(f *File) { delete(f.Accounts, id) })
}

// SetActive remembers the account most recently used.
func SetActive(path, id string) error {
	return Mutate(path, func(f *File) { f.Active = id })
}

// Blocked reports the entry keeping id out of rotation, if any.
func (f File) Blocked(id string, now time.Time) (Entry, bool) {
	e, ok := f.Accounts[id]
	if !ok || !e.blocking(now) {
		return Entry{}, false
	}
	return e, true
}

// Next chooses the account to continue on after current. It walks the registry
// order starting after current, preferring fully available accounts and
// falling back to one whose only problem is a single model's limit.
func Next(accts []registry.Account, st File, now time.Time, current string) (registry.Account, bool) {
	var fallback *registry.Account
	for _, a := range rotation(accts, current) {
		if a.ID == current {
			continue
		}
		e, blocked := st.Blocked(a.ID, now)
		switch {
		case !blocked:
			return a, true
		case e.Status == ModelLimited && fallback == nil:
			a := a
			fallback = &a
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return registry.Account{}, false
}

// rotation orders accts starting just after current (or from the start if unknown).
func rotation(accts []registry.Account, current string) []registry.Account {
	start := 0
	for i, a := range accts {
		if a.ID == current {
			start = i + 1
			break
		}
	}
	out := make([]registry.Account, 0, len(accts))
	for i := range accts {
		out = append(out, accts[(start+i)%len(accts)])
	}
	return out
}

// Start chooses the account for a fresh launch: the last active one if it is
// available, otherwise the next available. exhausted is set, with the account
// whose reset comes first, when nothing is available.
func Start(accts []registry.Account, st File, now time.Time) (acct registry.Account, exhausted bool, err error) {
	if len(accts) == 0 {
		return registry.Account{}, false, errors.New("no accounts registered")
	}
	for _, a := range accts {
		if a.ID == st.Active {
			if _, blocked := st.Blocked(a.ID, now); !blocked {
				return a, false, nil
			}
		}
	}
	if a, ok := Next(accts, st, now, st.Active); ok {
		return a, false, nil
	}
	if id, _, ok := EarliestReset(accts, st, now); ok {
		for _, a := range accts {
			if a.ID == id {
				return a, true, nil
			}
		}
	}
	return accts[0], true, nil
}

// EarliestReset finds the account that becomes usable soonest among those
// waiting on a timed limit.
func EarliestReset(accts []registry.Account, st File, now time.Time) (id string, at time.Time, ok bool) {
	for _, a := range accts {
		e, blocked := st.Blocked(a.ID, now)
		if !blocked || (e.Status != Exhausted && e.Status != ModelLimited) {
			continue
		}
		if !ok || e.Until.Before(at) {
			id, at, ok = a.ID, e.Until, true
		}
	}
	return id, at, ok
}
