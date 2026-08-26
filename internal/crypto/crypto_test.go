package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := bytes.Repeat([]byte{0x2a}, KeySize)
	c, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	plain := []byte(`{"username":"alice","password":"s3cr3t p@ss"}`)
	blob, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Ciphertext must not contain the plaintext.
	if bytes.Contains(blob, []byte("s3cr3t")) || bytes.Contains(blob, []byte("alice")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestDistinctNonces(t *testing.T) {
	c := newTestCipher(t)
	plain := []byte("same message")
	a, _ := c.Encrypt(plain)
	b, _ := c.Encrypt(plain)
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical blobs (nonce reuse)")
	}
	// Both still decrypt.
	for _, blob := range [][]byte{a, b} {
		if got, err := c.Decrypt(blob); err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("decrypt: %v / %q", err, got)
		}
	}
}

func TestWrongKeyFails(t *testing.T) {
	c1 := newTestCipher(t)
	c2, _ := New(bytes.Repeat([]byte{0x99}, KeySize))
	blob, _ := c1.Encrypt([]byte("secret"))
	if _, err := c2.Decrypt(blob); err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestTamperFails(t *testing.T) {
	c := newTestCipher(t)
	blob, _ := c.Encrypt([]byte("secret data here"))
	// Flip a bit in the ciphertext body (after the nonce).
	blob[len(blob)-1] ^= 0x01
	if _, err := c.Decrypt(blob); err == nil {
		t.Fatal("decrypt of tampered ciphertext should fail authentication")
	}
}

func TestShortCiphertext(t *testing.T) {
	c := newTestCipher(t)
	if _, err := c.Decrypt([]byte{0x00, 0x01}); err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
}

func TestKeySizeEnforced(t *testing.T) {
	if _, err := New(make([]byte, 16)); err != ErrKeySize {
		t.Fatalf("expected ErrKeySize for 16-byte key, got %v", err)
	}
	if _, err := New(make([]byte, KeySize)); err != nil {
		t.Fatalf("32-byte key should be accepted: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0x11}, KeySize)
	cases := []struct {
		name string
		enc  string
	}{
		{"hex", hex.EncodeToString(raw)},
		{"base64-std", base64.StdEncoding.EncodeToString(raw)},
		{"base64-rawstd", base64.RawStdEncoding.EncodeToString(raw)},
		{"base64-url", base64.URLEncoding.EncodeToString(raw)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseKey(tc.enc)
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("ParseKey mismatch")
			}
		})
	}
	// Wrong length / garbage.
	for _, bad := range []string{"", "zz", hex.EncodeToString(make([]byte, 16))} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("ParseKey(%q) should fail", bad)
		}
	}
}

func TestBase64Helpers(t *testing.T) {
	c := newTestCipher(t)
	plain := []byte("store me as text")
	s, err := c.EncryptToBase64(plain)
	if err != nil {
		t.Fatalf("EncryptToBase64: %v", err)
	}
	if s == "" {
		t.Fatal("empty base64 blob")
	}
	got, err := c.DecryptFromBase64(s)
	if err != nil {
		t.Fatalf("DecryptFromBase64: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("base64 round-trip mismatch")
	}
	if _, err := c.DecryptFromBase64("!!!not base64!!!"); err == nil {
		t.Fatal("expected base64 decode error")
	}
}

func TestNewFromStringAndGenerateKey(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	c, err := NewFromString(key)
	if err != nil {
		t.Fatalf("NewFromString(generated): %v", err)
	}
	blob, _ := c.Encrypt([]byte("x"))
	if got, _ := c.Decrypt(blob); string(got) != "x" {
		t.Fatal("round-trip via generated key failed")
	}
	if _, err := NewFromString("too-short"); err == nil {
		t.Fatal("NewFromString should reject an invalid key")
	}
}
