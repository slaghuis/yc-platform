// Package domain holds yc-preference's types and validation. It does no
// I/O.
package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Channel is a messaging channel.
type Channel string

// Channels.
const (
	ChannelEmail    Channel = "email"
	ChannelSMS      Channel = "sms"
	ChannelWhatsApp Channel = "whatsapp"
	ChannelTelegram Channel = "telegram"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelEmail, ChannelSMS, ChannelWhatsApp, ChannelTelegram:
		return true
	default:
		return false
	}
}

// Defaults are a shop's preferences for customers who have not chosen.
type Defaults struct {
	DisplayCurrency string
	Locale          string
	Newsletter      bool
	Marketing       bool
	NewProducts     bool
	UpdatedAt       time.Time
}

// Validate checks the defaults.
func (d Defaults) Validate() error {
	v := ycerrors.Validation()
	checkCurrency(v, "display_currency", d.DisplayCurrency)
	checkLocale(v, "locale", d.Locale)
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Preferences are one user's settings in one shop.
type Preferences struct {
	UserID          string
	DisplayCurrency string
	Locale          string
	PrimaryChannel  Channel // "" when none is chosen
	Newsletter      bool
	Marketing       bool
	NewProducts     bool
	Channels        []MessagingChannel
	// Stored is false for a user with no row yet: the values are the
	// shop's defaults.
	Stored    bool
	UpdatedAt time.Time
}

// Channel returns the user's c channel.
func (p Preferences) Channel(c Channel) (MessagingChannel, bool) {
	for _, mc := range p.Channels {
		if mc.Channel == c {
			return mc, true
		}
	}
	return MessagingChannel{}, false
}

// MessagingChannel is an address the user can be reached at. yc-message
// verifies it (docs/services/yc-preference.md, Channel Verification).
type MessagingChannel struct {
	Channel    Channel
	Address    string
	Verified   bool
	VerifiedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Patch changes some preferences; nil fields are left alone.
type Patch struct {
	DisplayCurrency *string
	Locale          *string
	PrimaryChannel  *Channel
	Newsletter      *bool
	Marketing       *bool
	NewProducts     *bool
}

// Validate checks the fields present. Whether the primary channel exists
// is checked against the stored channels.
func (p Patch) Validate() error {
	v := ycerrors.Validation()
	if p.DisplayCurrency != nil {
		checkCurrency(v, "display_currency", *p.DisplayCurrency)
	}
	if p.Locale != nil {
		checkLocale(v, "locale", *p.Locale)
	}
	if p.PrimaryChannel != nil && !p.PrimaryChannel.Valid() {
		v.Field("primary_channel", "invalid", "Must be email, sms, whatsapp or telegram")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Apply returns p applied to prefs.
func (p Patch) Apply(prefs Preferences) Preferences {
	if p.DisplayCurrency != nil {
		prefs.DisplayCurrency = *p.DisplayCurrency
	}
	if p.Locale != nil {
		prefs.Locale = *p.Locale
	}
	if p.PrimaryChannel != nil {
		prefs.PrimaryChannel = *p.PrimaryChannel
	}
	if p.Newsletter != nil {
		prefs.Newsletter = *p.Newsletter
	}
	if p.Marketing != nil {
		prefs.Marketing = *p.Marketing
	}
	if p.NewProducts != nil {
		prefs.NewProducts = *p.NewProducts
	}
	return prefs
}

// MarketingEqual reports whether the marketing flags match.
func MarketingEqual(a, b Preferences) bool {
	return a.Newsletter == b.Newsletter && a.Marketing == b.Marketing && a.NewProducts == b.NewProducts
}

var (
	// localeRe is a BCP 47 language tag of the common shape: language,
	// then optional script, region or variant subtags.
	localeRe   = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	e164Re     = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	telegramRe = regexp.MustCompile(`^@[A-Za-z0-9_]{5,32}$`)
)

// NormaliseAddress validates addr for channel c and returns its canonical
// form: email addresses lowercased, Telegram handles with a leading @.
func NormaliseAddress(c Channel, addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	v := ycerrors.Validation()
	switch c {
	case ChannelEmail:
		a, err := mail.ParseAddress(addr)
		if err != nil || a.Address != addr || a.Name != "" {
			v.Field("address", "invalid", "Must be an email address")
		}
		addr = strings.ToLower(addr)
	case ChannelSMS, ChannelWhatsApp:
		if !e164Re.MatchString(addr) {
			v.Field("address", "invalid", "Must be an E.164 phone number, e.g. +27831234567")
		}
	case ChannelTelegram:
		if !strings.HasPrefix(addr, "@") {
			addr = "@" + addr
		}
		if !telegramRe.MatchString(addr) {
			v.Field("address", "invalid", "Must be a Telegram username")
		}
	default:
		v.Field("channel", "invalid", "Must be email, sms, whatsapp or telegram")
	}
	if v.HasErrors() {
		return "", v.Build()
	}
	return addr, nil
}

func checkCurrency(v *ycerrors.ValidationBuilder, field, code string) {
	if !money.ValidCurrency(code) {
		v.Field(field, "invalid", "Must be an ISO 4217 currency code, e.g. ZAR")
	}
}

func checkLocale(v *ycerrors.ValidationBuilder, field, locale string) {
	if len(locale) > 35 || !localeRe.MatchString(locale) {
		v.Field(field, "invalid", "Must be a language tag, e.g. en-ZA")
	}
}
