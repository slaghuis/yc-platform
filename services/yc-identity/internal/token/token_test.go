package token_test

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/token"
)

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return k
}

var visitor = domain.User{ID: "0192f3a4-5b6c-7d8e-9f01-23456789abce", TenantID: "shop-a", Role: domain.RoleVisitor}

func TestIssueAndVerify(t *testing.T) {
	clock := testkit.NewFakeClock(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	s, err := token.NewSigner(newKey(t), "yc-identity", clock.Now)
	require.NoError(t, err)

	issued, err := s.Issue(visitor, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, clock.Now().Add(time.Hour), issued.ExpiresAt)
	assert.NotEmpty(t, issued.TokenID)

	got, err := s.Verify(issued.Token)
	require.NoError(t, err)
	assert.Equal(t, visitor.ID, got.UserID)
	assert.Equal(t, "shop-a", got.TenantID)
	assert.Equal(t, domain.RoleVisitor, got.Role)
	assert.Equal(t, issued.TokenID, got.TokenID)
	assert.Empty(t, got.Token)

	parsed, _, err := jwt.NewParser().ParseUnverified(issued.Token, &token.Claims{})
	require.NoError(t, err)
	assert.Equal(t, "RS256", parsed.Method.Alg())
	assert.NotEmpty(t, parsed.Header["kid"])
}

func TestVerifyRejects(t *testing.T) {
	clock := testkit.NewFakeClock(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	key := newKey(t)
	s, err := token.NewSigner(key, "yc-identity", clock.Now)
	require.NoError(t, err)
	issued, err := s.Issue(visitor, time.Hour)
	require.NoError(t, err)

	t.Run("expiry honours leeway", func(t *testing.T) {
		later := testkit.NewFakeClock(clock.Now().Add(time.Hour + 20*time.Second))
		v, err := token.NewSigner(key, "yc-identity", later.Now)
		require.NoError(t, err)
		_, err = v.Verify(issued.Token)
		require.NoError(t, err, "within leeway")

		later.Advance(time.Minute)
		_, err = v.Verify(issued.Token)
		assert.ErrorIs(t, err, token.ErrInvalid)
	})

	t.Run("signed by another key", func(t *testing.T) {
		other, err := token.NewSigner(newKey(t), "yc-identity", clock.Now)
		require.NoError(t, err)
		_, err = other.Verify(issued.Token)
		assert.ErrorIs(t, err, token.ErrInvalid)
	})

	t.Run("other issuer", func(t *testing.T) {
		forged, err := token.NewSigner(newKey(t), "someone-else", clock.Now)
		require.NoError(t, err)
		f, err := forged.Issue(visitor, time.Hour)
		require.NoError(t, err)
		_, err = s.Verify(f.Token)
		assert.ErrorIs(t, err, token.ErrInvalid)
	})

	t.Run("alg none", func(t *testing.T) {
		claims := token.Claims{TenantID: "shop-a", Role: "system_admin", ActorType: "system_admin"}
		claims.Subject, claims.ID, claims.Issuer = "x", "y", "yc-identity"
		claims.ExpiresAt = jwt.NewNumericDate(clock.Now().Add(time.Hour))
		raw, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)
		_, err = s.Verify(raw)
		assert.ErrorIs(t, err, token.ErrInvalid)
	})

	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(issued.Token, ".")
		parts[1] = parts[1][:len(parts[1])-2] + "AA"
		_, err := s.Verify(strings.Join(parts, "."))
		assert.ErrorIs(t, err, token.ErrInvalid)
	})

	t.Run("garbage", func(t *testing.T) {
		_, err := s.Verify("not-a-jwt")
		assert.ErrorIs(t, err, token.ErrInvalid)
	})
}
