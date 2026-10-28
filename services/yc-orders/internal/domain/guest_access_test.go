package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

func TestGuestAccessMintAndVerify(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	g := domain.GuestAccess{
		Secret: []byte("0123456789abcdef0123456789abcdef"),
		TTL:    time.Hour,
		Now:    func() time.Time { return now },
	}
	tenant, order := "shop-a", uuid.NewString()
	tok, exp, err := g.Mint(tenant, order)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Hour).UTC(), exp)
	assert.NoError(t, g.Verify(tenant, order, tok))

	assert.True(t, ycerrors.HasCode(g.Verify("shop-b", order, tok), ycerrors.CodeNotFound))
	assert.True(t, ycerrors.HasCode(g.Verify(tenant, uuid.NewString(), tok), ycerrors.CodeNotFound))
	assert.True(t, ycerrors.HasCode(g.Verify(tenant, order, tok+"x"), ycerrors.CodeNotFound))
	assert.True(t, ycerrors.HasCode(g.Verify(tenant, order, ""), ycerrors.CodeNotFound))

	g.Now = func() time.Time { return now.Add(time.Hour + time.Second) }
	assert.True(t, ycerrors.HasCode(g.Verify(tenant, order, tok), ycerrors.CodeNotFound), "expired")
}

func TestGuestAccessRejectsShortSecret(t *testing.T) {
	g := domain.GuestAccess{Secret: []byte("too-short"), TTL: time.Hour}
	_, _, err := g.Mint("shop-a", uuid.NewString())
	assert.Error(t, err)
}
