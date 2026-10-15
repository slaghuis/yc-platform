package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

func TestDefaultsRenderTheirSamples(t *testing.T) {
	for _, typ := range domain.Types {
		for _, ch := range []domain.Channel{domain.ChannelEmail, domain.ChannelSMS} {
			tpl, ok := domain.Default(typ, ch)
			require.True(t, ok, "%s/%s has a default", typ, ch)
			assert.True(t, tpl.Platform)
			require.NoError(t, tpl.Validate(), "%s/%s", typ, ch)
			r, err := tpl.Render(domain.Sample(typ))
			require.NoError(t, err)
			assert.NotContains(t, r.Body, "<no value>")
			if ch == domain.ChannelSMS {
				assert.LessOrEqual(t, len(r.Body), domain.MaxSMSLen, "%s sms fits", typ)
			}
		}
	}
}

func TestRenderMissingKeyFails(t *testing.T) {
	tpl := domain.Template{
		Type: domain.TypeWelcome, Channel: domain.ChannelEmail, Locale: "en",
		Subject: "Hi", Body: "Hello {{.full_name}}", Format: domain.FormatText,
	}
	_, err := tpl.Render(domain.Sample(domain.TypeWelcome))
	require.Error(t, err)
	err = tpl.Validate()
	require.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))
}

func TestRenderHTMLEscapes(t *testing.T) {
	tpl := domain.Template{
		Type: domain.TypeWelcome, Channel: domain.ChannelEmail, Locale: "en",
		Subject: "Hi {{.name}}", Body: "<p>Hello {{.name}}</p>", Format: domain.FormatHTML,
	}
	r, err := tpl.Render(map[string]any{"name": "<b>Ann</b>"})
	require.NoError(t, err)
	assert.Equal(t, "<p>Hello &lt;b&gt;Ann&lt;/b&gt;</p>", r.Body)
	assert.Equal(t, "Hi <b>Ann</b>", r.Subject)
}

func TestValidateRules(t *testing.T) {
	base := domain.Template{
		Type: domain.TypeWelcome, Channel: domain.ChannelSMS, Locale: "en-ZA",
		Body: "Hi {{.name}}", Format: domain.FormatText,
	}
	require.NoError(t, base.Validate())

	for name, mutate := range map[string]func(*domain.Template){
		"sms subject":    func(t *domain.Template) { t.Subject = "x" },
		"sms html":       func(t *domain.Template) { t.Format = domain.FormatHTML },
		"sms too long":   func(t *domain.Template) { t.Body = strings.Repeat("x", domain.MaxSMSLen+1) },
		"email no subj":  func(t *domain.Template) { t.Channel = domain.ChannelEmail },
		"bad locale":     func(t *domain.Template) { t.Locale = "EN_za" },
		"unknown type":   func(t *domain.Template) { t.Type = "promo" },
		"whatsapp":       func(t *domain.Template) { t.Channel = domain.ChannelWhatsApp },
		"empty body":     func(t *domain.Template) { t.Body = " " },
		"unknown format": func(t *domain.Template) { t.Format = "markdown" },
	} {
		tpl := base
		mutate(&tpl)
		assert.True(t, ycerrors.HasCode(tpl.Validate(), ycerrors.CodeValidationFailed), name)
	}
}

func TestResolve(t *testing.T) {
	all := func(domain.Channel) bool { return true }
	emailOnly := func(ch domain.Channel) bool { return ch == domain.ChannelEmail }
	c := domain.Contact{
		UserID: "u1", Primary: domain.ChannelSMS, Locale: "af",
		Channels: []domain.ChannelAddress{
			{Channel: domain.ChannelEmail, Address: "a@x.test"},
			{Channel: domain.ChannelSMS, Address: "+27820000000", Verified: true},
		},
	}
	r, ok := c.Resolve(all)
	require.True(t, ok)
	assert.Equal(t, domain.Recipient{UserID: "u1", Channel: domain.ChannelSMS, Address: "+27820000000", Locale: "af"}, r)

	r, ok = c.Resolve(emailOnly)
	require.True(t, ok)
	assert.Equal(t, domain.ChannelEmail, r.Channel, "no SMS driver: email")

	c.Channels[1].Verified = false
	r, _ = c.Resolve(all)
	assert.Equal(t, domain.ChannelEmail, r.Channel, "unverified primary: email, even unverified")

	r, ok = domain.GuestContact("", "+27820000001").Resolve(all)
	require.True(t, ok)
	assert.Equal(t, domain.ChannelSMS, r.Channel, "guest without email: SMS")

	_, ok = domain.GuestContact("", "+27820000001").Resolve(emailOnly)
	assert.False(t, ok)
	_, ok = domain.Contact{}.Resolve(all)
	assert.False(t, ok)
}

func TestBackoff(t *testing.T) {
	base := 30 * time.Second
	assert.Equal(t, 30*time.Second, domain.Backoff(base, 1))
	assert.Equal(t, time.Minute, domain.Backoff(base, 2))
	assert.Equal(t, 4*time.Minute, domain.Backoff(base, 4))
	assert.Equal(t, time.Hour, domain.Backoff(base, 20))
}

func TestRedact(t *testing.T) {
	m := domain.Message{
		Type: domain.TypePasswordReset, Rendered: domain.Rendered{Subject: "Reset", Body: "https://x/reset?t=secret"},
		Context: map[string]any{"reset_url": "https://x/reset?t=secret"},
	}
	m.Redact()
	assert.Equal(t, domain.Redacted, m.Body)
	assert.Equal(t, "Reset", m.Subject)
	assert.NotContains(t, m.Context, "reset_url")

	w := domain.Message{Type: domain.TypeWelcome, Rendered: domain.Rendered{Body: "Hi"}}
	w.Redact()
	assert.Equal(t, "Hi", w.Body)
}

func TestLocaleCandidates(t *testing.T) {
	assert.Equal(t, []string{"en-ZA", "en"}, domain.LocaleCandidates("en-ZA", "en"))
	assert.Equal(t, []string{"af-ZA", "af", "en"}, domain.LocaleCandidates("af-ZA", "en"))
	assert.Equal(t, []string{"en"}, domain.LocaleCandidates("", "en"))
}

func TestVerificationCode(t *testing.T) {
	code, err := domain.NewCode()
	require.NoError(t, err)
	assert.Len(t, code, domain.CodeDigits)

	now := time.Now()
	v := domain.Verification{ID: "v1", ExpiresAt: now.Add(time.Minute)}
	v.CodeHash = domain.HashCode(v.ID, code)
	assert.NotContains(t, v.CodeHash, code)
	assert.True(t, v.Matches(code))
	assert.True(t, v.Matches(" "+code+" "))
	assert.False(t, v.Matches("x"+code))
	assert.NotEqual(t, v.CodeHash, domain.HashCode("v2", code), "salted by ID")

	assert.True(t, v.Usable(now))
	assert.False(t, v.Usable(now.Add(2*time.Minute)), "expired")
	v.Attempts = domain.MaxCodeAttempts
	assert.False(t, v.Usable(now), "too many attempts")
	v.Attempts = 0
	v.ConfirmedAt = &now
	assert.False(t, v.Usable(now), "confirmed")
}
