package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

// fieldsOf returns the fields named by a validation error.
func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	require.Error(t, err)
	e, ok := ycerrors.As(err)
	require.True(t, ok, "not a ycerrors error: %v", err)
	require.Equal(t, ycerrors.CodeValidationFailed, e.Code)
	out := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestValidHostname(t *testing.T) {
	long := strings.Repeat("a", 63)
	tests := []struct {
		host string
		want bool
	}{
		{"acme.localhost", true},
		{"localhost", true},
		{"shop-1.example.co.za", true},
		{"xn--80ak6aa92e.com", true},
		{long + ".com", true},
		{long + "a.com", false}, // label over 63
		{strings.Repeat("a.", 127) + "a", false},
		{"", false},
		{"Acme.localhost", false}, // must be normalised first
		{"acme.localhost:8070", false},
		{"-acme.localhost", false},
		{"acme-.localhost", false},
		{"acme..localhost", false},
		{".acme.localhost", false},
		{"acme.localhost.", false},
		{"acme_shop.localhost", false},
		{"acme shop.localhost", false},
		{"https://acme.localhost", false},
		{"acme.localhost/path", false},
		{"*.acme.localhost", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			assert.Equal(t, tc.want, domain.ValidHostname(tc.host))
		})
	}
}

func TestNormalizeHostnames(t *testing.T) {
	got, v := domain.NormalizeHostnames("hostnames", []string{" A.Test ", "b.test"})
	assert.False(t, v.HasErrors())
	assert.Equal(t, []string{"a.test", "b.test"}, got)

	tests := []struct {
		name  string
		hosts []string
	}{
		{"none", nil},
		{"duplicate after lowercasing", []string{"a.test", "A.TEST"}},
		{"invalid", []string{"a.test", "b c"}},
		{"too many", func() []string {
			hs := make([]string, 0, domain.MaxHostnames+1)
			for i := range domain.MaxHostnames + 1 {
				hs = append(hs, strings.Repeat("h", i+1)+".test")
			}
			return hs
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, v := domain.NormalizeHostnames("hostnames", tc.hosts)
			assert.True(t, v.HasErrors())
		})
	}
}

func TestValidateTenantID(t *testing.T) {
	tests := []struct {
		id string
		ok bool
	}{
		{"acme", true},
		{"yc-demo", true},
		{"shop-1", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"_platform", false},
		{"Acme", false},
		{"acme_shop", false},
		{"acme.shop", false},
		{"-acme", false},
		{"acme-", false},
		{"acme--shop", false},
		{"acme shop", false},
		{"acme*", false},
		{"acme>", false},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			err := domain.ValidateTenantID(tc.id)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.Equal(t, []string{"tenant_id"}, fieldsOf(t, err))
		})
	}
}

func TestCreateInputFieldErrors(t *testing.T) {
	valid := func() domain.CreateInput {
		return domain.CreateInput{
			TenantID: "acme", Name: "Acme", Hostnames: []string{"acme.localhost"}, StoreCurrency: "ZAR",
			DefaultJurisdiction: "ZA", ContactEmail: "ops@acme.test",
		}
	}
	_, err := valid().Validate()
	require.NoError(t, err)

	tests := []struct {
		name  string
		edit  func(in *domain.CreateInput)
		field string
	}{
		{"blank name", func(in *domain.CreateInput) { in.Name = "  " }, "name"},
		{"long name", func(in *domain.CreateInput) { in.Name = strings.Repeat("n", domain.MaxNameLength+1) }, "name"},
		{"lowercase currency", func(in *domain.CreateInput) { in.StoreCurrency = "zar" }, "store_currency"},
		{"long currency", func(in *domain.CreateInput) { in.StoreCurrency = "ZARR" }, "store_currency"},
		{"no currency", func(in *domain.CreateInput) { in.StoreCurrency = "" }, "store_currency"},
		{"lowercase jurisdiction", func(in *domain.CreateInput) { in.DefaultJurisdiction = "za" }, "default_jurisdiction"},
		{"long jurisdiction", func(in *domain.CreateInput) { in.DefaultJurisdiction = "ZAF" }, "default_jurisdiction"},
		{"no contact email", func(in *domain.CreateInput) { in.ContactEmail = "" }, "contact_email"},
		{"bad contact email", func(in *domain.CreateInput) { in.ContactEmail = "nope" }, "contact_email"},
		{"display name in email", func(in *domain.CreateInput) { in.ContactEmail = "Ops <ops@acme.test>" }, "contact_email"},
		{"bad admin email", func(in *domain.CreateInput) { in.PrimaryAdminEmail = "nope" }, "primary_admin_email"},
		{"no hostnames", func(in *domain.CreateInput) { in.Hostnames = nil }, "hostnames"},
		{"bad hostname", func(in *domain.CreateInput) { in.Hostnames = []string{"a b"} }, "hostnames"},
		{"reserved id", func(in *domain.CreateInput) { in.TenantID = "_platform" }, "tenant_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := valid()
			tc.edit(&in)
			_, err := in.Validate()
			assert.Equal(t, []string{tc.field}, fieldsOf(t, err))
		})
	}

	t.Run("reports every problem at once", func(t *testing.T) {
		_, err := domain.CreateInput{TenantID: "Bad", StoreCurrency: "x"}.Validate()
		assert.ElementsMatch(t,
			[]string{"tenant_id", "hostnames", "name", "contact_email", "store_currency", "default_jurisdiction"},
			fieldsOf(t, err))
	})

	t.Run("optional admin email is trimmed", func(t *testing.T) {
		in := valid()
		in.PrimaryAdminEmail = "  boss@acme.test "
		got, err := in.Validate()
		require.NoError(t, err)
		assert.Equal(t, "boss@acme.test", got.PrimaryAdminEmail)
	})
}

func TestHostnamesInputPrimary(t *testing.T) {
	tests := []struct {
		name    string
		in      domain.HostnamesInput
		want    []string
		primary string
		field   string
	}{
		{"defaults to first", domain.HostnamesInput{Hostnames: []string{"b.test", "a.test"}}, []string{"b.test", "a.test"}, "b.test", ""},
		{
			"named primary moves first",
			domain.HostnamesInput{Hostnames: []string{"a.test", "b.test", "c.test"}, Primary: "C.test"},
			[]string{"c.test", "a.test", "b.test"},
			"c.test", "",
		},
		{"primary not listed", domain.HostnamesInput{Hostnames: []string{"a.test"}, Primary: "b.test"}, nil, "", "primary_hostname"},
		{"no hostnames", domain.HostnamesInput{Primary: "a.test"}, nil, "", "hostnames"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.Validate()
			if tc.field != "" {
				assert.Equal(t, []string{tc.field}, fieldsOf(t, err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, domain.HostnameNames(got))
			assert.True(t, got[0].IsPrimary)
			for _, h := range got[1:] {
				assert.False(t, h.IsPrimary)
			}
			assert.Equal(t, tc.primary, domain.Tenant{Hostnames: got}.PrimaryHostname())
		})
	}
}

func TestPatchInputRules(t *testing.T) {
	s := func(v string) *string { return &v }
	tests := []struct {
		name  string
		in    domain.PatchInput
		field string
	}{
		{"nothing to change", domain.PatchInput{}, "body"},
		{"blank name", domain.PatchInput{Name: s(" ")}, "name"},
		{"bad email", domain.PatchInput{ContactEmail: s("x")}, "contact_email"},
		{"blank email", domain.PatchInput{ContactEmail: s("")}, "contact_email"},
		{"bad vat", domain.PatchInput{VATNumber: s("123")}, "vat_number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.in.Validate()
			assert.Equal(t, []string{tc.field}, fieldsOf(t, err))
		})
	}
}

func TestValidateReasonLimits(t *testing.T) {
	_, err := domain.ValidateReason(strings.Repeat("é", domain.MaxReasonLength))
	assert.NoError(t, err, "the limit counts characters, not bytes")
	_, err = domain.ValidateReason(strings.Repeat("é", domain.MaxReasonLength+1))
	assert.Equal(t, []string{"reason"}, fieldsOf(t, err))
}

func TestListFilterRules(t *testing.T) {
	f, err := domain.ListFilter{Status: domain.StatusActive, Search: "  acme "}.Validate()
	require.NoError(t, err)
	assert.Equal(t, "acme", f.Search)

	_, err = domain.ListFilter{Status: "gone"}.Validate()
	assert.Equal(t, []string{"status"}, fieldsOf(t, err))
	_, err = domain.ListFilter{Search: strings.Repeat("q", domain.MaxSearchLength+1)}.Validate()
	assert.Equal(t, []string{"q"}, fieldsOf(t, err))
}

func TestStatusValid(t *testing.T) {
	for _, s := range []domain.Status{domain.StatusActive, domain.StatusSuspended, domain.StatusDeleted} {
		assert.True(t, s.Valid(), s)
	}
	for _, s := range []domain.Status{"", "Active", "purged"} {
		assert.False(t, s.Valid(), s)
	}
}
