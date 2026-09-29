// Package auth issues and verifies API keys.
//
// The model follows GitHub's personal access tokens:
//
//   - A key is shown exactly once, at creation. Only a SHA-256 hash is stored.
//   - Every key keeps a display prefix and its last four characters.
//   - Access is scoped; keys can expire and be revoked.
//
// On top of that a key belongs either to one tenant (tenant key) or to none
// (platform key). A tenant key can only ever see its own tenant: the tenant is
// taken from the key, never from the request.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const (
	// TokenPrefix marks a key so it can be recognised in logs and by secret
	// scanners.
	TokenPrefix = "bk_"
	tokenBytes  = 32
	// prefixLength is how much of the token is stored in the clear.
	prefixLength   = len(TokenPrefix) + 8
	lastFourLength = 4
)

// Generated is a freshly minted key. Token is the only time the secret exists
// outside the caller's hands.
type Generated struct {
	Token    string
	Hash     string
	Prefix   string
	LastFour string
}

// Generate mints a new key.
func Generate() (*Generated, error) {
	buf := make([]byte, tokenBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, fmt.Errorf("auth: read entropy: %w", err)
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return &Generated{
		Token:    token,
		Hash:     Hash(token),
		Prefix:   token[:prefixLength],
		LastFour: token[len(token)-lastFourLength:],
	}, nil
}

// Hash is the stored value. A single SHA-256 is right for 256 bits of uniform
// randomness: there is no dictionary to run, so a KDF would only cost latency.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// LooksLikeToken lets the authenticator skip a database round trip for junk.
func LooksLikeToken(token string) bool {
	return strings.HasPrefix(token, TokenPrefix) && len(token) >= prefixLength+lastFourLength
}

// Display renders a key for humans: bk_AbCdEfGh...wxyz.
func Display(prefix, lastFour string) string {
	return prefix + "..." + lastFour
}

// fixedTimeEqual compares secrets without leaking their length or content
// through timing.
func fixedTimeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
