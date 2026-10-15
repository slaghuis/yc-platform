package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Verification code rules (ADR-023 rule 5).
const (
	CodeDigits         = 6
	MaxCodeAttempts    = 5
	VerificationResend = time.Minute
)

// Verification is a code sent to prove a user owns a channel address.
type Verification struct {
	ID          string
	TenantID    string
	UserID      string
	Channel     Channel
	Address     string
	CodeHash    string
	Attempts    int
	ExpiresAt   time.Time
	ConfirmedAt *time.Time
	CreatedAt   time.Time
}

// NewCode returns a random numeric code.
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", CodeDigits, n.Int64()), nil
}

// HashCode hashes a code with its verification's ID, so equal codes of
// different verifications hash differently.
func HashCode(verificationID, code string) string {
	sum := sha256.Sum256([]byte(verificationID + ":" + strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// Matches reports whether code is the verification's code.
func (v Verification) Matches(code string) bool {
	return subtle.ConstantTimeCompare([]byte(HashCode(v.ID, code)), []byte(v.CodeHash)) == 1
}

// Usable reports whether the code can still be tried at now.
func (v Verification) Usable(now time.Time) bool {
	return v.ConfirmedAt == nil && now.Before(v.ExpiresAt) && v.Attempts < MaxCodeAttempts
}
