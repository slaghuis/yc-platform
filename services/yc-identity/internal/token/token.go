// Package token issues and verifies yc-identity's RS256 access tokens.
package token

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// leeway tolerates clock skew between yc-identity and token holders.
const leeway = 30 * time.Second

// Claims are the access token claims (docs/services/yc-identity.md, Token
// Design). sub is the user ID and jti the token ID.
type Claims struct {
	TenantID  string `json:"tenant_id"`
	Role      string `json:"role"`
	ActorType string `json:"actor_type"`
	jwt.RegisteredClaims
}

// ErrInvalid is returned for any token that fails verification. The cause
// is wrapped for logs; callers must not reveal it to clients.
var ErrInvalid = errors.New("token: invalid")

// Signer issues and verifies tokens with one RSA key pair.
type Signer struct {
	private *rsa.PrivateKey
	public  *rsa.PublicKey
	kid     string
	issuer  string
	now     func() time.Time
}

// NewSigner returns a Signer. now is the clock for iat, exp and
// verification; pass time.Now outside tests.
func NewSigner(private *rsa.PrivateKey, issuer string, now func() time.Time) (*Signer, error) {
	if private == nil {
		return nil, fmt.Errorf("token: private key is required")
	}
	if issuer == "" {
		return nil, fmt.Errorf("token: issuer is required")
	}
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("token: marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return &Signer{
		private: private,
		public:  &private.PublicKey,
		kid:     base64.RawURLEncoding.EncodeToString(sum[:8]),
		issuer:  issuer,
		now:     now,
	}, nil
}

// LoadPrivateKey reads a PEM-encoded RSA private key (PKCS#1 or PKCS#8).
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is operator configuration
	if err != nil {
		return nil, fmt.Errorf("token: read private key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(b)
	if err != nil {
		return nil, fmt.Errorf("token: parse private key %s: %w", path, err)
	}
	return key, nil
}

// Issue signs a token for user that expires after ttl.
func (s *Signer) Issue(user domain.User, ttl time.Duration) (domain.Session, error) {
	now := s.now()
	claims := Claims{
		TenantID:  user.TenantID,
		Role:      string(user.Role),
		ActorType: string(user.Role.ActorType()),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   user.ID,
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = s.kid
	signed, err := t.SignedString(s.private)
	if err != nil {
		return domain.Session{}, fmt.Errorf("token: sign: %w", err)
	}
	return domain.Session{
		Token:     signed,
		TokenID:   claims.ID,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		Role:      user.Role,
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

// Verify checks the signature, algorithm, issuer and expiry and returns
// the session the token carries, without the raw token. Every failure wraps
// ErrInvalid.
func (s *Signer) Verify(raw string) (domain.Session, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return s.public, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return domain.Session{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if claims.Subject == "" || claims.ID == "" || claims.TenantID == "" {
		return domain.Session{}, fmt.Errorf("%w: missing sub, jti or tenant_id", ErrInvalid)
	}
	if claims.ActorType != claims.Role {
		return domain.Session{}, fmt.Errorf("%w: actor_type %q does not match role %q", ErrInvalid, claims.ActorType, claims.Role)
	}
	return domain.Session{
		TokenID:   claims.ID,
		UserID:    claims.Subject,
		TenantID:  claims.TenantID,
		Role:      domain.Role(claims.Role),
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}
