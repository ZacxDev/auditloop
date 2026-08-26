// Package apikey provides the read-API key machinery: minting a per-user,
// read-only API key (32 bytes of crypto/rand → base64url), storing ONLY its
// sha256 hash, and verifying a presented key in constant time.
//
// It deliberately MIRRORS the P5 plugin push-token pattern (internal/plugin/
// token.go) rather than sharing a package: the two token kinds have different
// scopes and lifecycles (a plugin token authorizes PUSH to one target; an API
// key authorizes read-only pulls scoped to a user), and keeping them separate
// avoids coupling the read-API feature to the plugin ingestion package. The
// primitives are identical (~15 lines) and each is unit-tested independently.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// tokenBytes is the number of random bytes in an API key before base64url
// encoding. 32 bytes = 256 bits of entropy.
const tokenBytes = 32

// Generate mints a new API key: 32 cryptographically-random bytes encoded as an
// unpadded base64url string (the plaintext, shown to the user ONCE), plus its
// hex-encoded sha256 hash (the ONLY thing stored at rest — keys are not
// reversible from the DB). Callers store the hash and return the token to the
// user a single time.
func Generate() (token, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, Hash(token), nil
}

// Hash returns the hex-encoded sha256 of a token. A presented key is hashed and
// looked up against the stored hash — the plaintext never touches the database.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqual reports whether two hex hashes are equal in constant time,
// so verification latency does not leak how much of the hash matched.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
