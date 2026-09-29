// Package secretbox encrypts small secrets at rest (provider credentials) with
// AES-256-GCM. The key comes from SECRETS_KEY; losing it makes stored
// credentials unreadable, so back it up with the database password.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Box seals and opens secrets.
type Box struct{ aead cipher.AEAD }

// ErrNoKey means SECRETS_KEY is unset where one is required.
var ErrNoKey = errors.New("secretbox: SECRETS_KEY is not set")

// New derives a box from a key. A 64-character hex or 44-character base64
// value is used as the raw 32-byte key; any other string of at least 32
// characters is hashed with SHA-256.
func New(key string) (*Box, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrNoKey
	}
	var raw []byte
	if b, err := hex.DecodeString(key); err == nil && len(b) == 32 {
		raw = b
	} else if b, err := base64.StdEncoding.DecodeString(key); err == nil && len(b) == 32 {
		raw = b
	} else if len(key) >= 32 {
		sum := sha256.Sum256([]byte(key))
		raw = sum[:]
	} else {
		return nil, fmt.Errorf("secretbox: SECRETS_KEY must be 32 random bytes (hex or base64) or at least 32 characters")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; aad binds it to its row (e.g. the account id) so a
// ciphertext copied into another row does not decrypt.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts what Seal produced.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(ciphertext) < n {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	return b.aead.Open(nil, ciphertext[:n], ciphertext[n:], aad)
}
