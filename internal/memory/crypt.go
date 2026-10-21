package memory

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// KeyLen is the size of a memory key in bytes (AES-256).
const KeyLen = 32

// sealAAD binds every encrypted line to this format, so a ciphertext cannot be
// replayed into some other use of the same key.
var sealAAD = []byte("claude-go-memory/1")

// Cipher seals and opens log lines with AES-256-GCM. A sealed line is one JSON
// object, {"enc":1,"n":<nonce>,"c":<ciphertext>}, so an encrypted log is still
// one entry per line and can sit next to plain lines written before encryption
// was turned on.
type Cipher struct{ aead cipher.AEAD }

// GenerateKey returns a fresh random key.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate memory key: %w", err)
	}
	return key, nil
}

// NewCipher builds a Cipher from a KeyLen-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("memory key must be %d bytes, got %d", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("memory cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("memory cipher: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

type envelope struct {
	Enc int    `json:"enc"`
	N   string `json:"n"`
	C   string `json:"c"`
}

// isSealed reports whether a log line is an encrypted envelope.
func isSealed(line []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte(`{"enc":`))
}

// Seal encrypts plain into one envelope line (without a trailing newline).
func (c *Cipher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("memory nonce: %w", err)
	}
	enc := base64.RawStdEncoding
	return json.Marshal(envelope{
		Enc: 1,
		N:   enc.EncodeToString(nonce),
		C:   enc.EncodeToString(c.aead.Seal(nil, nonce, plain, sealAAD)),
	})
}

var errCannotOpen = errors.New("cannot decrypt (wrong key, or the line was changed)")

// Open decrypts one envelope line.
func (c *Cipher) Open(line []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(line), &env); err != nil || env.Enc != 1 {
		return nil, errCannotOpen
	}
	enc := base64.RawStdEncoding
	nonce, err := enc.DecodeString(env.N)
	if err != nil || len(nonce) != c.aead.NonceSize() {
		return nil, errCannotOpen
	}
	ct, err := enc.DecodeString(env.C)
	if err != nil {
		return nil, errCannotOpen
	}
	plain, err := c.aead.Open(nil, nonce, ct, sealAAD)
	if err != nil {
		return nil, errCannotOpen
	}
	return plain, nil
}
