// Package password hashes and verifies passwords with Argon2id
// (docs/services/yc-identity.md, Password Storage).
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	saltLen = 16
	keyLen  = 32
)

// Params are the Argon2id cost parameters.
type Params struct {
	Time     uint32 `koanf:"time"`
	MemoryKB uint32 `koanf:"memory_kb"`
	Threads  uint8  `koanf:"threads"`
}

// Validate rejects parameters too weak to use.
func (p Params) Validate() error {
	if p.Time < 1 || p.MemoryKB < 8*1024 || p.Threads < 1 {
		return fmt.Errorf("password: argon2 params too weak: time %d, memory %d KiB, threads %d", p.Time, p.MemoryKB, p.Threads)
	}
	return nil
}

// ErrMalformed is returned for a stored hash that is not a PHC-format
// Argon2id string.
var ErrMalformed = errors.New("password: malformed hash")

// Hasher hashes with fixed parameters.
type Hasher struct {
	p     Params
	dummy string
}

// NewHasher returns a Hasher using p.
func NewHasher(p Params) (*Hasher, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	h := &Hasher{p: p}
	dummy, err := h.Hash("timing-equaliser")
	if err != nil {
		return nil, err
	}
	h.dummy = dummy
	return h, nil
}

// Hash returns the PHC string for pw:
// $argon2id$v=19$m=<KiB>,t=<time>,p=<threads>$<salt>$<key>.
func (h *Hasher) Hash(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, h.p.Time, h.p.MemoryKB, h.p.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.p.MemoryKB, h.p.Time, h.p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether pw matches encoded, using the parameters stored
// in encoded. The comparison is constant-time.
func (h *Hasher) Verify(pw, encoded string) (bool, error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.MemoryKB, p.Threads, uint32(len(key))) //nolint:gosec // len(key) is keyLen-sized
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// VerifyNothing spends the same time as a Verify against a real hash, so a
// login for an unknown email is not faster than one with a wrong password.
func (h *Hasher) VerifyNothing(pw string) {
	_, _ = h.Verify(pw, h.dummy)
}

// NeedsRehash reports whether encoded was made with other parameters.
func (h *Hasher) NeedsRehash(encoded string) bool {
	p, _, _, err := decode(encoded)
	return err != nil || p != h.p
}

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrMalformed
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, ErrMalformed
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKB, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, ErrMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformed
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, ErrMalformed
	}
	return p, salt, key, nil
}
