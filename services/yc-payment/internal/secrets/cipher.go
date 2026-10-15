// Package secrets encrypts gateway credentials at rest with AES-256-GCM
// under the service's cipher key (YC_PAYMENT_CIPHER_KEY).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Cipher seals and opens credential blobs. The associated data binds a
// blob to its row, so a blob copied to another tenant does not open.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher returns a Cipher for a base64-encoded 32-byte key.
func NewCipher(keyB64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher key is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: cipher key is %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext; the result is nonce || ciphertext.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// ErrOpen means a blob was tampered with, or sealed under another key or
// for another row.
var ErrOpen = errors.New("secrets: cannot decrypt")

// Open decrypts a blob made by Seal with the same associated data.
func (c *Cipher) Open(blob, aad []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(blob) < n {
		return nil, ErrOpen
	}
	out, err := c.aead.Open(nil, blob[:n], blob[n:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}
