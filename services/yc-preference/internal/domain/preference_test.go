package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
)

func ptr[T any](v T) *T { return &v }

func TestPatchValidate(t *testing.T) {
	require.NoError(t, domain.Patch{}.Validate())
	require.NoError(t, domain.Patch{
		DisplayCurrency: ptr("USD"), Locale: ptr("en-ZA"), PrimaryChannel: ptr(domain.ChannelSMS),
	}.Validate())
	require.NoError(t, domain.Patch{Locale: ptr("zh-Hant-TW")}.Validate())

	for name, p := range map[string]domain.Patch{
		"lower-case currency": {DisplayCurrency: ptr("usd")},
		"empty locale":        {Locale: ptr("")},
		"bad locale":          {Locale: ptr("English")},
		"unknown channel":     {PrimaryChannel: ptr(domain.Channel("pigeon"))},
	} {
		assert.Error(t, p.Validate(), name)
	}
}

func TestPatchApply(t *testing.T) {
	p := domain.Preferences{DisplayCurrency: "ZAR", Locale: "en-ZA", PrimaryChannel: domain.ChannelEmail}
	got := domain.Patch{DisplayCurrency: ptr("EUR")}.Apply(p)
	assert.Equal(t, "EUR", got.DisplayCurrency)
	assert.Equal(t, "en-ZA", got.Locale)
	assert.Equal(t, domain.ChannelEmail, got.PrimaryChannel)
}

func TestNormaliseAddress(t *testing.T) {
	for _, tc := range []struct {
		channel   domain.Channel
		in, want  string
		wantError bool
	}{
		{domain.ChannelEmail, " Bob@Example.com ", "bob@example.com", false},
		{domain.ChannelEmail, "Bob <bob@example.com>", "", true},
		{domain.ChannelEmail, "not-an-email", "", true},
		{domain.ChannelSMS, "+27831234567", "+27831234567", false},
		{domain.ChannelWhatsApp, "0831234567", "", true},
		{domain.ChannelTelegram, "yc_shopper", "@yc_shopper", false},
		{domain.ChannelTelegram, "@ab", "", true},
		{domain.Channel("fax"), "123", "", true},
	} {
		got, err := domain.NormaliseAddress(tc.channel, tc.in)
		if tc.wantError {
			assert.Error(t, err, "%s %q", tc.channel, tc.in)
			continue
		}
		require.NoError(t, err, "%s %q", tc.channel, tc.in)
		assert.Equal(t, tc.want, got)
	}
}

func TestDefaultsValidate(t *testing.T) {
	require.NoError(t, domain.Defaults{DisplayCurrency: "ZAR", Locale: "en-ZA"}.Validate())
	assert.Error(t, domain.Defaults{DisplayCurrency: "", Locale: "en-ZA"}.Validate())
}
