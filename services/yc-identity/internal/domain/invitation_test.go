package domain

import (
	"strings"
	"testing"
	"time"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
)

func TestNewInviteInput(t *testing.T) {
	in, err := NewInviteInput("_platform", "  Ann@Example.COM ", "system_admin", " Ann ", "Admin.Localhost")
	if err != nil {
		t.Fatal(err)
	}
	if in.TenantID != dbkit.PlatformTenantID || in.Email != "ann@example.com" || in.Role != RoleSystemAdmin {
		t.Errorf("got %+v", in)
	}
	if in.DisplayName != "Ann" || in.AcceptHost != "admin.localhost" {
		t.Errorf("got %+v", in)
	}

	shop, err := NewInviteInput("acme", "owner@acme.test", "shop_admin", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if shop.Role != RoleShopAdmin || shop.TenantID != "acme" {
		t.Errorf("got %+v", shop)
	}

	bad := []struct{ tenant, email, role string }{
		{"", "a@b.c", "system_admin"},
		{"acme", "not-email", "shop_admin"},
		{"acme", "a@b.c", "system_admin"},
		{"_platform", "a@b.c", "shop_admin"},
		{"acme", "a@b.c", "customer"},
		{"acme", "a@b.c", "shop_admin"}, // display name alone is fine; oversized checked below
	}
	for _, b := range bad[:5] {
		if _, err := NewInviteInput(b.tenant, b.email, b.role, "", ""); err == nil {
			t.Errorf("NewInviteInput(%q, %q, %q) succeeded", b.tenant, b.email, b.role)
		}
	}
	if _, err := NewInviteInput("acme", "a@b.c", "shop_admin", strings.Repeat("n", 101), ""); err == nil {
		t.Error("oversized display_name succeeded")
	}
}

func TestNewAcceptInviteInput(t *testing.T) {
	in, err := NewAcceptInviteInput("tok", "0123456789", "  Name ")
	if err != nil {
		t.Fatal(err)
	}
	if in.Token != "tok" || in.DisplayName != "Name" {
		t.Errorf("got %+v", in)
	}
	if _, err := NewAcceptInviteInput("tok", "short", ""); err == nil {
		t.Error("short password succeeded")
	}
}

func TestInvitationEffectiveStatus(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	pending := Invitation{Status: InvitationPending, ExpiresAt: now.Add(time.Hour)}
	if pending.EffectiveStatus(now) != InvitationPending {
		t.Error("live pending should stay pending")
	}
	expired := Invitation{Status: InvitationPending, ExpiresAt: now.Add(-time.Minute)}
	if expired.EffectiveStatus(now) != InvitationExpired {
		t.Error("past-due pending should be expired")
	}
	revoked := Invitation{Status: InvitationRevoked, ExpiresAt: now.Add(-time.Hour)}
	if revoked.EffectiveStatus(now) != InvitationRevoked {
		t.Error("revoked stays revoked")
	}
}
