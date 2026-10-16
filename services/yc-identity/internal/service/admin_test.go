package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

func TestSuspendGuards(t *testing.T) {
	s := &Service{}

	t.Run("non system admin forbidden", func(t *testing.T) {
		ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
			TenantID: "acme", ActorID: "u1", ActorType: authctx.ActorShopAdmin,
		})
		_, err := s.Suspend(ctx, "u2", "")
		require.Error(t, err)
		assert.Equal(t, 403, statusOf(err))
	})

	t.Run("self suspend refused", func(t *testing.T) {
		ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
			TenantID: "_platform", ActorID: "admin-1", ActorType: authctx.ActorSystemAdmin,
		})
		_, err := s.Suspend(ctx, "admin-1", "")
		require.Error(t, err)
		assert.Equal(t, 409, statusOf(err))
	})
}

func TestDemoteGuards(t *testing.T) {
	s := &Service{}
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "acme", ActorID: "u1", ActorType: authctx.ActorShopAdmin,
	})
	_, err := s.Demote(ctx, "u2")
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(err))
}

func TestReactivateGuards(t *testing.T) {
	s := &Service{}
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "acme", ActorID: "u1", ActorType: authctx.ActorCustomer,
	})
	_, err := s.Reactivate(ctx, "u2")
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(err))
}

func TestCreateInvitationGuards(t *testing.T) {
	s := &Service{}
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "_platform", ActorID: "admin-1", ActorType: authctx.ActorShopAdmin,
	})
	_, err := s.CreateInvitation(ctx, domain.InviteInput{
		TenantID: "_platform", Email: "a@b.c", Role: domain.RoleSystemAdmin,
	})
	require.Error(t, err)
	assert.Equal(t, 403, statusOf(err))
}

func statusOf(err error) int {
	if e, ok := ycerrors.As(err); ok {
		return e.Status
	}
	return -1
}
