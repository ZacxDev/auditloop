// Package crypto provides authenticated encryption-at-rest for sensitive
// values (P4 login-recipe credentials). It uses AES-256-GCM with a random nonce
// prepended to each ciphertext, keyed by a single 32-byte server-side key
// (AUDITLOOP_ENCRYPTION_KEY). The key never reaches the browser and the
// plaintext (credentials) is never logged.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

// ErrKeySize is returned when a key is not exactly 32 bytes.
var ErrKeySize = fmt.Errorf("crypto: encryption key must be %d bytes", KeySize)

// Cipher performs AES-256-GCM encrypt/decrypt with a fixed key.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a raw 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// NewFromString builds a Cipher from a key encoded as hex (64 chars) or base64
// (standard or raw, with or without padding) that decodes to exactly 32 bytes.
func NewFromString(s string) (*Cipher, error) {
	key, err := ParseKey(s)
	if err != nil {
		return nil, err
	}
	return New(key)
}

// ParseKey decodes a hex- or base64-encoded 32-byte key. Hex is tried first when
// the input looks like hex; otherwise base64 (std then raw-url) is attempted.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("crypto: empty key")
	}
	// Hex: exactly 64 lowercase/uppercase hex chars.
	if len(s) == KeySize*2 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	// Base64 variants.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("crypto: key must decode (hex or base64) to %d bytes", KeySize)
}

// Encrypt returns nonce||ciphertext for plaintext. Each call uses a fresh random
// nonce, so encrypting the same plaintext twice yields different blobs.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Seal appends the ciphertext to nonce, so the result is nonce||ct||tag.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. A tampered or wrong-key blob fails GCM
// authentication and returns an error (never partial plaintext).
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("crypto: decryption failed (wrong key or tampered data)")
	}
	return pt, nil
}

// EncryptToBase64 encrypts and standard-base64-encodes (portable TEXT storage).
func (c *Cipher) EncryptToBase64(plaintext []byte) (string, error) {
	blob, err := c.Encrypt(plaintext)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// DecryptFromBase64 reverses EncryptToBase64.
func (c *Cipher) DecryptFromBase64(s string) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("crypto: invalid base64 ciphertext")
	}
	return c.Decrypt(blob)
}

// GenerateKey returns a fresh random 32-byte key hex-encoded (helper for ops /
// tests to mint AUDITLOOP_ENCRYPTION_KEY). Never used to auto-generate a prod key.
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
