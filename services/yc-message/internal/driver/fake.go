package driver

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// FakeSMSDomain is where the fake SMS driver delivers: an SMS to
// +27831234567 arrives in the dev mailbox as 27831234567@sms.test.
const FakeSMSDomain = "sms.test"

// FakeSMS stands in for an SMS provider in development: it delivers each
// SMS as an email through the SMTP driver, so MailHog shows it. Delivery
// reports come through the fake webhook.
type FakeSMS struct {
	Mail *SMTP
}

// Name is "fake-sms".
func (FakeSMS) Name() string { return "fake-sms" }

// Channel is SMS.
func (FakeSMS) Channel() domain.Channel { return domain.ChannelSMS }

// Send mails the SMS to the number's dev mailbox.
func (f FakeSMS) Send(ctx context.Context, m Outgoing) (string, error) {
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, m.To)
	if digits == "" {
		return "", fmt.Errorf("%w: %q is not a phone number", ErrPermanent, m.To)
	}
	return f.Mail.Send(ctx, Outgoing{
		To: digits + "@" + FakeSMSDomain, Subject: "SMS to " + m.To, Body: m.Body, Format: domain.FormatText,
	})
}
