// Package plugin defines the P5 plugin-push ingestion contract: the push schema
// (an ingestion-oriented mirror of report.PageReport), its strict validation, a
// mapper turning a validated payload + uploaded files into the run/pages/findings
// the DB layer expects, the push-token machinery (generate → store sha256 →
// constant-time verify → rotate), and a generic multipart uploader used by the
// reference CLI (cmd/auditloop-push).
//
// A plugin target (targets.auth_mode="plugin") is PUSH-ONLY: auditloop never
// crawls it. An external harness POSTs a completed run's artifacts to
// POST /api/plugins/runs authenticated by the target's bearer push token, and the
// results flow into the SAME dashboard — including P2 regression diffing against
// the previous push.
package plugin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// tokenBytes is the number of random bytes in a push token before base64url
// encoding. 32 bytes = 256 bits of entropy.
const tokenBytes = 32

// GenerateToken mints a new push token: 32 cryptographically-random bytes encoded
// as an unpadded base64url string (the plaintext, shown to the user ONCE), plus
// its hex-encoded sha256 hash (the ONLY thing stored at rest — tokens are never
// reversible from the DB). Callers store the hash and return the token to the
// user a single time.
func GenerateToken() (token, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken returns the hex-encoded sha256 of a token. The presented token is
// hashed and looked up against the stored hash — the plaintext never touches the
// database.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyToken reports whether presenting `token` matches a stored hash, using a
// constant-time comparison of the two hex hashes (crypto/subtle) so verification
// latency does not leak how much of the hash matched.
func VerifyToken(storedHash, token string) bool {
	return ConstantTimeEqual(storedHash, HashToken(token))
}

// ConstantTimeEqual reports whether two hex hashes are equal in constant time.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
