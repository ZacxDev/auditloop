package apikey

import "testing"

func TestGenerateProducesDistinctTokensAndMatchingHash(t *testing.T) {
	tok1, hash1, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok2, hash2, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if tok1 == "" || tok2 == "" {
		t.Fatal("empty token")
	}
	if tok1 == tok2 {
		t.Fatal("expected distinct tokens (crypto/rand)")
	}
	if hash1 == hash2 {
		t.Fatal("expected distinct hashes")
	}
	// The plaintext must never equal its own hash (i.e. it IS hashed).
	if tok1 == hash1 {
		t.Fatal("token stored in plaintext form")
	}
	// Hash is deterministic and matches.
	if Hash(tok1) != hash1 {
		t.Fatal("Hash(token) != returned hash")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	tok, hash, _ := Generate()
	if !ConstantTimeEqual(hash, Hash(tok)) {
		t.Fatal("correct token should verify")
	}
	// Wrong token → different hash → no match (rotation invalidates the old key).
	otherTok, _, _ := Generate()
	if ConstantTimeEqual(hash, Hash(otherTok)) {
		t.Fatal("a different token must not verify")
	}
	if ConstantTimeEqual(hash, Hash(tok+"x")) {
		t.Fatal("a tampered token must not verify")
	}
}
