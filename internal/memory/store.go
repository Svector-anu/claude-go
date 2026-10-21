// Package memory is claude-go's local project memory: short facts that Claude
// can store and recall, shared by every account and every session.
//
// Storage is an append-only JSONL log per machine (<machine>.jsonl). Each
// machine only ever appends to its own file, so a folder synced between
// computers (Syncthing, iCloud, a git repository) merges without conflicts:
// readers combine every file and let the newest operation per (scope, key)
// win. Nothing here depends on a database or a network.
package memory

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Svector-anu/claude-go/internal/fsx"
)

const (
	opVersion = 1

	MaxKeyLen   = 128
	MaxValueLen = 2000 // runes
	MaxTags     = 8
	MaxTagLen   = 32
	MaxFacts    = 5000

	lockTimeout    = 5 * time.Second
	compactAbove   = 1 << 20 // bytes
	tombstoneKeep  = 90 * 24 * time.Hour
	importFileName = "import.jsonl"
	machineFile    = "machine"
	maxLineBytes   = 1 << 20
)

// Operation kinds.
const (
	KindUpsert = "upsert"
	KindDelete = "delete"
)

var (
	keyPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9:_./-]*$`)
	tagPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	scopePattern = regexp.MustCompile(`^project:[a-z0-9][a-z0-9:_./@+-]{0,199}$`)
)

// Op is one line of the log.
type Op struct {
	V       int       `json:"v"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Machine string    `json:"machine"`
	Kind    string    `json:"op"`
	Scope   string    `json:"scope"`
	Key     string    `json:"key"`
	Value   string    `json:"value,omitempty"`
	Tags    []string  `json:"tags,omitempty"`
}

// Fact is a live (not deleted) memory.
type Fact struct {
	Scope     string
	Key       string
	Value     string
	Tags      []string
	UpdatedAt time.Time
	Machine   string
}

// newer orders operations: later time wins; ties break on machine then id so
// every reader picks the same winner.
func (a Op) newer(b Op) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	if a.Machine != b.Machine {
		return a.Machine > b.Machine
	}
	return a.ID > b.ID
}

type fileState struct {
	offset int64
	// locked counts encrypted lines that could not be read (no key, wrong key or tampered).
	locked int
}

// Store reads and appends the memory log in one directory. It is safe for
// concurrent use and for several processes sharing the directory.
type Store struct {
	Dir     string
	Machine string
	Now     func() time.Time
	// Max caps live facts; zero means MaxFacts.
	Max int
	// Cipher, when set, seals everything this store writes and opens sealed
	// lines it reads. Lines written without one stay readable.
	Cipher *Cipher

	mu    sync.Mutex
	files map[string]*fileState
	ops   map[string]Op // winning operation per scope+key
	seen  map[string]bool
}

// Open prepares dir (mode 0700) and this machine's identity. The identity is a
// random id, not a hostname, so nothing about the computer leaves it.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create memory dir: %w", err)
	}
	id, err := machineID(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Machine: id, Now: time.Now}
	s.reset()
	return s, nil
}

func machineID(dir string) (string, error) {
	path := filepath.Join(dir, machineFile)
	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(id) {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read machine id: %w", err)
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	if err := fsx.WriteFileAtomic(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) reset() {
	s.files = map[string]*fileState{}
	s.ops = map[string]Op{}
	s.seen = map[string]bool{}
}

func mapKey(scope, key string) string { return scope + "\x00" + key }

// ValidateScope accepts "global" or "project:<id>".
func ValidateScope(scope string) error {
	if scope == GlobalScope || scopePattern.MatchString(scope) {
		return nil
	}
	return fmt.Errorf("invalid scope %q", scope)
}

// NormalizeKey lower-cases and checks a fact key.
func NormalizeKey(key string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || len(key) > MaxKeyLen || !keyPattern.MatchString(key) {
		return "", fmt.Errorf("key must be 1-%d characters of a-z, 0-9, ':', '_', '.', '/' or '-', starting with a letter or digit (for example \"auth:strategy\")", MaxKeyLen)
	}
	return key, nil
}

func normalizeTags(tags []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		if len(t) > MaxTagLen || !tagPattern.MatchString(t) {
			return nil, fmt.Errorf("tag %q must be 1-%d characters of a-z, 0-9, '_' or '-'", t, MaxTagLen)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, fmt.Errorf("at most %d tags", MaxTags)
	}
	return out, nil
}

// validValue checks a value's size and content.
func validValue(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", errors.New("value is empty")
	case utf8.RuneCountInString(v) > MaxValueLen:
		return "", fmt.Errorf("value is longer than %d characters; store a shorter summary", MaxValueLen)
	case strings.ContainsRune(v, 0) || !utf8.ValidString(v):
		return "", errors.New("value contains invalid text")
	}
	return v, nil
}

// rejectSecrets refuses credentials in a fact's key, value and tags.
func rejectSecrets(key, value, tags string) error {
	check := func(what string, ok bool) error {
		if ok {
			return fmt.Errorf("not stored: the text looks like %s. Never store credentials; store a description of where the secret lives instead", what)
		}
		return nil
	}
	if what, ok := looksLikeSecretKey(key); ok {
		return check(what, true)
	}
	if what, ok := LooksLikeSecret(value); ok {
		return check(what, true)
	}
	if what, ok := LooksLikeSecret(tags); ok {
		return check(what, true)
	}
	return nil
}

// stampAfter returns the time to give a new operation for a key: now, but
// always strictly later than the operation it replaces, so a write (or a
// deletion) can never lose to the state its author just saw, however the
// clocks of the machines compare.
func (s *Store) stampAfter(cur Op, exists bool) time.Time {
	at := s.Now().UTC()
	if exists && !at.After(cur.At) {
		at = cur.At.Add(time.Microsecond)
	}
	return at
}

func (s *Store) ownFile() string { return filepath.Join(s.Dir, s.Machine+".jsonl") }

func (s *Store) logFiles() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("read memory dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(s.Dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// refresh folds new log lines into memory. A file that shrank (compacted by
// its owner) forces a full rebuild.
func (s *Store) refresh() error {
	files, err := s.logFiles()
	if err != nil {
		return err
	}
	for _, p := range files {
		if st, ok := s.files[p]; ok {
			if fi, err := os.Stat(p); err == nil && fi.Size() < st.offset {
				s.reset()
				return s.refresh()
			}
		}
	}
	for _, p := range files {
		if err := s.readFile(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readFile(path string) error {
	st := s.files[path]
	if st == nil {
		st = &fileState{}
		s.files[path] = st
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a partial last line is picked up on the next refresh
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		st.offset += int64(len(line))
		if len(line) > maxLineBytes {
			continue
		}
		op, sealed, ok := s.decode(line)
		if sealed && !ok {
			st.locked++
		}
		if ok {
			s.apply(op)
		}
	}
	return nil
}

// decode reads one log line. sealed says it was an encrypted line; ok says it
// yielded a valid operation.
func (s *Store) decode(line []byte) (op Op, sealed, ok bool) {
	raw := bytes.TrimSpace(line)
	if isSealed(raw) {
		if s.Cipher == nil {
			return Op{}, true, false
		}
		plain, err := s.Cipher.Open(raw)
		if err != nil {
			return Op{}, true, false
		}
		raw = plain
		sealed = true
	}
	if json.Unmarshal(raw, &op) != nil || validOp(op) != nil {
		return Op{}, sealed, false
	}
	return op, sealed, true
}

// encode renders an operation as one log line, sealed when c is set.
func encode(op Op, c *Cipher) ([]byte, error) {
	line, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("encode operation: %w", err)
	}
	if c == nil {
		return line, nil
	}
	return c.Seal(line)
}

// validOp is applied to everything read from disk, including files written by
// other machines, so a damaged or hostile file cannot inject odd data.
func validOp(op Op) error {
	if op.V != opVersion || op.ID == "" || op.At.IsZero() || op.Machine == "" {
		return errors.New("incomplete operation")
	}
	if err := ValidateScope(op.Scope); err != nil {
		return err
	}
	if _, err := NormalizeKey(op.Key); err != nil {
		return err
	}
	switch op.Kind {
	case KindDelete:
		return nil
	case KindUpsert:
		if _, err := validValue(op.Value); err != nil {
			return err
		}
		_, err := normalizeTags(op.Tags)
		return err
	}
	return fmt.Errorf("unknown operation %q", op.Kind)
}

func (s *Store) apply(op Op) {
	s.seen[op.ID] = true
	k := mapKey(op.Scope, op.Key)
	if cur, ok := s.ops[k]; ok && !op.newer(cur) {
		return
	}
	s.ops[k] = op
}

func newOpID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// withLock runs fn holding the directory lock, with fresh state.
func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := fsx.AcquireLock(filepath.Join(s.Dir, ".lock"), lockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	if err := s.refresh(); err != nil {
		return err
	}
	return fn()
}

func (s *Store) appendOp(op Op) error {
	line, err := encode(op, s.Cipher)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.ownFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open memory log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append to memory log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close memory log: %w", err)
	}
	return nil
}

func (s *Store) maxFacts() int {
	if s.Max > 0 {
		return s.Max
	}
	return MaxFacts
}

func (s *Store) liveCount() int {
	n := 0
	for _, op := range s.ops {
		if op.Kind == KindUpsert {
			n++
		}
	}
	return n
}

func factOf(op Op) Fact {
	return Fact{Scope: op.Scope, Key: op.Key, Value: op.Value, Tags: op.Tags, UpdatedAt: op.At, Machine: op.Machine}
}

// Remember stores (or updates) a fact. Storing the same value and tags again
// changes nothing.
func (s *Store) Remember(scope, key, value string, tags []string) (Fact, error) {
	if err := ValidateScope(scope); err != nil {
		return Fact{}, err
	}
	key, err := NormalizeKey(key)
	if err != nil {
		return Fact{}, err
	}
	if value, err = validValue(value); err != nil {
		return Fact{}, err
	}
	if tags, err = normalizeTags(tags); err != nil {
		return Fact{}, err
	}
	if err := rejectSecrets(key, value, strings.Join(tags, " ")); err != nil {
		return Fact{}, err
	}

	var out Fact
	err = s.withLock(func() error {
		cur, exists := s.ops[mapKey(scope, key)]
		if exists && cur.Kind == KindUpsert && cur.Value == value && strings.Join(cur.Tags, ",") == strings.Join(tags, ",") {
			out = factOf(cur)
			return nil
		}
		if !(exists && cur.Kind == KindUpsert) && s.liveCount() >= s.maxFacts() {
			return fmt.Errorf("memory is full (%d facts); forget some first", s.maxFacts())
		}
		id, err := newOpID()
		if err != nil {
			return err
		}
		op := Op{V: opVersion, ID: id, At: s.stampAfter(cur, exists), Machine: s.Machine, Kind: KindUpsert, Scope: scope, Key: key, Value: value, Tags: tags}
		if err := s.appendOp(op); err != nil {
			return err
		}
		s.apply(op)
		out = factOf(op)
		return s.maybeCompact()
	})
	return out, err
}

// Forget deletes a fact. It reports whether the fact existed.
func (s *Store) Forget(scope, key string) (bool, error) {
	if err := ValidateScope(scope); err != nil {
		return false, err
	}
	key, err := NormalizeKey(key)
	if err != nil {
		return false, err
	}
	existed := false
	err = s.withLock(func() error {
		cur, ok := s.ops[mapKey(scope, key)]
		if !ok || cur.Kind != KindUpsert {
			return nil
		}
		existed = true
		id, err := newOpID()
		if err != nil {
			return err
		}
		op := Op{V: opVersion, ID: id, At: s.stampAfter(cur, true), Machine: s.Machine, Kind: KindDelete, Scope: scope, Key: key}
		if err := s.appendOp(op); err != nil {
			return err
		}
		s.apply(op)
		return s.maybeCompact()
	})
	return existed, err
}

// Facts returns every live fact in the given scopes (all scopes when none are named), newest first.
func (s *Store) Facts(scopes ...string) ([]Fact, error) {
	var out []Fact
	err := s.withLock(func() error {
		want := map[string]bool{}
		for _, sc := range scopes {
			want[sc] = true
		}
		for _, op := range s.ops {
			if op.Kind == KindUpsert && (len(want) == 0 || want[op.Scope]) {
				out = append(out, factOf(op))
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Scope+out[i].Key < out[j].Scope+out[j].Key
	})
	return out, err
}

// maybeCompact rewrites this machine's file once it grows large, keeping only
// operations that still win (and recent deletions, so they keep overriding
// older copies of the fact in other machines' files). Called with the lock held.
func (s *Store) maybeCompact() error {
	fi, err := os.Stat(s.ownFile())
	if err != nil || fi.Size() < compactAbove {
		return nil
	}
	var keep []Op
	cutoff := s.Now().Add(-tombstoneKeep)
	for _, op := range s.ops {
		if op.Machine != s.Machine {
			continue
		}
		if op.Kind == KindDelete && op.At.Before(cutoff) {
			continue
		}
		keep = append(keep, op)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[j].newer(keep[i]) })
	var buf bytes.Buffer
	for _, op := range keep {
		line, err := encode(op, s.Cipher)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := fsx.WriteFileAtomic(s.ownFile(), buf.Bytes(), 0o600); err != nil {
		return err
	}
	s.reset()
	return s.refresh()
}

// Export writes every winning operation (live facts and recent deletions) as a
// JSONL bundle that Import can merge into another store.
func (s *Store) Export(w io.Writer) (int, error) {
	n := 0
	err := s.withLock(func() error {
		var ops []Op
		for _, op := range s.ops {
			ops = append(ops, op)
		}
		sort.Slice(ops, func(i, j int) bool { return ops[j].newer(ops[i]) })
		for _, op := range ops {
			line, err := json.Marshal(op)
			if err != nil {
				return fmt.Errorf("encode operation: %w", err)
			}
			if _, err := w.Write(append(line, '\n')); err != nil {
				return fmt.Errorf("write export: %w", err)
			}
			n++
		}
		return nil
	})
	return n, err
}

// ImportResult says what Import did with a bundle.
type ImportResult struct{ Added, Skipped, Rejected int }

// Import merges a bundle. Operations already known are skipped, anything
// malformed or secret-like is rejected, and the rest keep their original
// time and machine so last-writer-wins still decides correctly. Importing the
// same bundle twice changes nothing.
func (s *Store) Import(r io.Reader) (ImportResult, error) {
	var res ImportResult
	err := s.withLock(func() error {
		var buf bytes.Buffer
		var applied []Op
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
		for sc.Scan() {
			raw := bytes.TrimSpace(sc.Bytes())
			if len(raw) == 0 {
				continue
			}
			var op Op
			if json.Unmarshal(raw, &op) != nil || validOp(op) != nil || rejectSecrets(op.Key, op.Value, strings.Join(op.Tags, " ")) != nil {
				res.Rejected++
				continue
			}
			if s.seen[op.ID] {
				res.Skipped++
				continue
			}
			line, err := encode(op, s.Cipher)
			if err != nil {
				return err
			}
			buf.Write(line)
			buf.WriteByte('\n')
			applied = append(applied, op)
			s.seen[op.ID] = true // dedupe within the bundle itself
			res.Added++
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read bundle: %w", err)
		}
		if buf.Len() == 0 {
			return nil
		}
		f, err := os.OpenFile(filepath.Join(s.Dir, importFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open import log: %w", err)
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			_ = f.Close()
			return fmt.Errorf("write import log: %w", err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		for _, op := range applied {
			s.apply(op)
		}
		return nil
	})
	return res, err
}

// Locked reports how many encrypted lines in the folder could not be read: no
// key on this machine, a different key, or a line that was changed.
func (s *Store) Locked() (int, error) {
	n := 0
	err := s.withLock(func() error {
		for _, st := range s.files {
			n += st.locked
		}
		return nil
	})
	return n, err
}

// Rewrite re-encodes this machine's own files (its log and its imports) with
// target: sealed with a Cipher, or plain when target is nil. Other machines'
// files are not touched. Every line is kept; nothing is compacted. A sealed
// line that cannot be opened with the current Cipher stops the rewrite, so no
// note is lost. On success the store uses target from then on.
func (s *Store) Rewrite(target *Cipher) error {
	return s.withLock(func() error {
		for _, p := range []string{s.ownFile(), filepath.Join(s.Dir, importFileName)} {
			data, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", p, err)
			}
			var out bytes.Buffer
			for _, line := range bytes.Split(data, []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				op, sealed, ok := s.decode(line)
				if !ok {
					if sealed {
						return fmt.Errorf("%s has an encrypted line this machine's key cannot open; nothing was changed", p)
					}
					continue // a damaged plain line was already ignored when reading
				}
				enc, err := encode(op, target)
				if err != nil {
					return err
				}
				out.Write(enc)
				out.WriteByte('\n')
			}
			if err := fsx.WriteFileAtomic(p, out.Bytes(), 0o600); err != nil {
				return err
			}
		}
		s.Cipher = target
		s.reset()
		return s.refresh()
	})
}
