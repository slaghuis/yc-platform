package domain

import (
	"strings"
	"testing"
)

func TestNewRegistration(t *testing.T) {
	reg, err := NewRegistration("  Ann@Example.COM ", "0123456789", "  Ann ")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Email != "ann@example.com" || reg.DisplayName != "Ann" {
		t.Errorf("got %+v", reg)
	}

	bad := []struct{ email, pw, name string }{
		{"not-an-email", "0123456789", ""},
		{"Ann <ann@example.com>", "0123456789", ""},
		{"ann@example.com", "short", ""},
		{"ann@example.com", strings.Repeat("x", MaxPasswordLen+1), ""},
		{"ann@example.com", "0123456789", strings.Repeat("n", 101)},
	}
	for _, b := range bad {
		if _, err := NewRegistration(b.email, b.pw, b.name); err == nil {
			t.Errorf("NewRegistration(%q, %d chars, %d chars) succeeded", b.email, len(b.pw), len(b.name))
		}
	}
}

func TestRoleOrder(t *testing.T) {
	if !RoleShopAdmin.Above(RoleCustomer) || RoleCustomer.Above(RoleShopAdmin) || RoleCustomer.Above(RoleCustomer) {
		t.Error("role order is wrong")
	}
	if Role("root").Valid() || !RoleVisitor.Valid() {
		t.Error("Valid is wrong")
	}
}
