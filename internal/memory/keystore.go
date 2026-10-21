package memory

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Svector-anu/claude-go/internal/fsx"
)

// ErrNoKey means no memory key is stored: encryption is off on this machine.
var ErrNoKey = errors.New("no memory key")

// KeyStore keeps the memory key. The key is never written into the notes folder,
// so a synced or backed-up folder holds ciphertext only.
type KeyStore interface {
	Load() ([]byte, error) // ErrNoKey when none is stored
	Save(key []byte) error
	Delete() error // not an error when none is stored
}

func decodeKey(text string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(key) != KeyLen {
		return nil, errors.New("the stored memory key is damaged")
	}
	return key, nil
}

// FileKeyStore keeps the key, hex encoded, in a file only its owner can read.
// This protects a notes folder that is copied or synced elsewhere; it does not
// protect against another process running as the same user.
type FileKeyStore struct{ Path string }

func (f FileKeyStore) Load() ([]byte, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Path, err)
	}
	return decodeKey(string(data))
}

func (f FileKeyStore) Save(key []byte) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(f.Path), err)
	}
	return fsx.WriteFileAtomic(f.Path, []byte(hex.EncodeToString(key)+"\n"), 0o600)
}

func (f FileKeyStore) Delete() error {
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", f.Path, err)
	}
	return nil
}

// securityRunner runs /usr/bin/security with args, feeding stdin, and returns its stdout.
type securityRunner func(stdin string, args ...string) (stdout string, exitCode int, err error)

// KeychainKeyStore keeps the key as a generic password in the macOS login
// keychain, so it is protected by the user's login and kept out of dotfiles. It
// talks to /usr/bin/security, the same program that created the item, so
// reading it back needs no prompt. The key reaches the program on standard
// input, never in its arguments, where `ps` would show it.
type KeychainKeyStore struct {
	Service, Account string
	run              securityRunner
}

// NewKeychainKeyStore returns a store backed by the macOS keychain.
func NewKeychainKeyStore(service, account string) KeychainKeyStore {
	return KeychainKeyStore{Service: service, Account: account, run: runSecurity}
}

func runSecurity(stdin string, args ...string) (string, int, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode(), nil
	}
	return out.String(), 0, err
}

const keychainItemNotFound = 44

func (k KeychainKeyStore) Load() ([]byte, error) {
	out, code, err := k.run("", "find-generic-password", "-s", k.Service, "-a", k.Account, "-w")
	if err != nil {
		return nil, fmt.Errorf("run security: %w", err)
	}
	if code == keychainItemNotFound {
		return nil, ErrNoKey
	}
	if code != 0 {
		return nil, fmt.Errorf("the keychain would not give up the memory key (security exit %d)", code)
	}
	return decodeKey(out)
}

func (k KeychainKeyStore) Save(key []byte) error {
	cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", k.Service, k.Account, hex.EncodeToString(key))
	if _, _, err := k.run(cmd, "-i"); err != nil {
		return fmt.Errorf("run security: %w", err)
	}
	// `security -i` does not report failures through its exit status; read the key back.
	got, err := k.Load()
	if err != nil || !bytes.Equal(got, key) {
		return errors.New("could not save the memory key in the keychain")
	}
	return nil
}

func (k KeychainKeyStore) Delete() error {
	_, code, err := k.run("", "delete-generic-password", "-s", k.Service, "-a", k.Account)
	if err != nil {
		return fmt.Errorf("run security: %w", err)
	}
	if code != 0 && code != keychainItemNotFound {
		return fmt.Errorf("could not remove the memory key from the keychain (security exit %d)", code)
	}
	return nil
}
