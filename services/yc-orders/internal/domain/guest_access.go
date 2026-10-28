package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Guest access tokens are signed, time-limited capabilities for a single
// placed guest order (issue #22). They let the storefront open the order
// from an email on a device that does not have the original visitor cookie.
// The raw token is never stored or published on the bus.

const (
	guestAccessVersion = "v1"
	// DefaultGuestAccessTTL is how long a guest email link works.
	DefaultGuestAccessTTL = 30 * 24 * time.Hour
	// MinGuestAccessSecretLen matches service-client secrets (ADR-021).
	MinGuestAccessSecretLen = 32
)

// GuestAccess signs and verifies guest order tokens.
type GuestAccess struct {
	Secret []byte
	TTL    time.Duration
	Now    func() time.Time
}

// Mint returns a URL-safe token for tenantID/orderID and its expiry.
func (g GuestAccess) Mint(tenantID, orderID string) (token string, exp time.Time, err error) {
	if err := g.ready(); err != nil {
		return "", time.Time{}, err
	}
	if tenantID == "" || orderID == "" {
		return "", time.Time{}, ycerrors.Validation().
			Field("order_id", "invalid", "Order id is required").Build()
	}
	now := g.now()
	exp = now.Add(g.TTL).UTC().Truncate(time.Second)
	payload := guestPayload(tenantID, orderID, exp.Unix())
	mac := g.mac(payload)
	token = guestAccessVersion + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac)
	return token, exp, nil
}

// Verify reports whether token authorises tenantID/orderID at the current time.
func (g GuestAccess) Verify(tenantID, orderID, token string) error {
	if err := g.ready(); err != nil {
		return err
	}
	invalid := ycerrors.NotFound("order", orderID)
	ver, rest, ok := strings.Cut(token, ".")
	if !ok || ver != guestAccessVersion {
		return invalid
	}
	payloadB64, macB64, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(macB64, ".") {
		return invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return invalid
	}
	mac, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return invalid
	}
	want := g.mac(string(payload))
	if subtle.ConstantTimeCompare(mac, want) != 1 {
		return invalid
	}
	parts := strings.Split(string(payload), "\n")
	if len(parts) != 3 {
		return invalid
	}
	expUnix, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return invalid
	}
	if parts[0] != tenantID || parts[1] != orderID {
		return invalid
	}
	if !g.now().Before(time.Unix(expUnix, 0)) {
		return invalid
	}
	return nil
}

func (g GuestAccess) ready() error {
	if len(g.Secret) < MinGuestAccessSecretLen {
		return fmt.Errorf("guest access secret must be at least %d bytes", MinGuestAccessSecretLen)
	}
	if g.TTL <= 0 {
		return fmt.Errorf("guest access ttl must be positive")
	}
	return nil
}

func (g GuestAccess) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g GuestAccess) mac(payload string) []byte {
	m := hmac.New(sha256.New, g.Secret)
	_, _ = m.Write([]byte(payload))
	return m.Sum(nil)
}

func guestPayload(tenantID, orderID string, expUnix int64) string {
	return tenantID + "\n" + orderID + "\n" + strconv.FormatInt(expUnix, 10)
}
