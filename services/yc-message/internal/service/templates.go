package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/repository"
)

// TemplateChannels are the channels templates can be written for.
var TemplateChannels = []domain.Channel{domain.ChannelEmail, domain.ChannelSMS}

// TemplateKey names a template.
type TemplateKey struct {
	Type    domain.Type
	Channel domain.Channel
	Locale  string
}

func (k TemplateKey) validate() error {
	v := ycerrors.Validation()
	if !k.Type.Valid() {
		v.Field("type", "invalid", "Unknown message type")
	}
	if k.Channel != domain.ChannelEmail && k.Channel != domain.ChannelSMS {
		v.Field("channel", "invalid", "Templates are for email or sms")
	}
	if !domain.ValidLocale(k.Locale) {
		v.Field("locale", "invalid", "Locale is a language tag such as en or en-ZA")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Templates lists the shop's own templates and the platform defaults they
// override or fall back to.
func (s *Service) Templates(ctx context.Context) ([]domain.Template, error) {
	var out []domain.Template
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		own, err := s.repo.Templates(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		for _, t := range domain.Types {
			for _, ch := range TemplateChannels {
				if tpl, ok := domain.Default(t, ch); ok {
					out = append(out, tpl)
				}
			}
		}
		out = append(out, own...)
		return nil
	})
	return out, err
}

// Template returns the template the shop sends for a key: its own, or the
// platform default it falls back to.
func (s *Service) Template(ctx context.Context, k TemplateKey) (domain.Template, error) {
	if err := k.validate(); err != nil {
		return domain.Template{}, err
	}
	var tpl domain.Template
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		tpl, err = s.template(ctx, tx, tenantID, k.Type, k.Channel, k.Locale)
		return err
	})
	return tpl, err
}

// PutTemplate creates or replaces the shop's template for a key, after
// checking it renders against a sample message.
func (s *Service) PutTemplate(ctx context.Context, tpl domain.Template) (domain.Template, error) {
	if err := (TemplateKey{tpl.Type, tpl.Channel, tpl.Locale}).validate(); err != nil {
		return domain.Template{}, err
	}
	if tpl.Format == "" {
		tpl.Format = domain.FormatText
	}
	if err := tpl.Validate(); err != nil {
		return domain.Template{}, err
	}
	var out domain.Template
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		tpl.TenantID, tpl.Platform, tpl.UpdatedAt = tenantID, false, s.now().UTC()
		var err error
		out, err = s.repo.PutTemplate(ctx, tx, tpl)
		return err
	})
	return out, err
}

// DeleteTemplate removes the shop's template, so the default applies again.
func (s *Service) DeleteTemplate(ctx context.Context, k TemplateKey) error {
	if err := k.validate(); err != nil {
		return err
	}
	return s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		ok, err := s.repo.DeleteTemplate(ctx, tx, tenantID, k.Type, k.Channel, k.Locale)
		if err == nil && !ok {
			err = ycerrors.NotFound("template", string(k.Type)+"/"+string(k.Channel)+"/"+k.Locale)
		}
		return err
	})
}

// TestSend sends the template for a key, filled with sample values, to an
// address of the admin's choice.
func (s *Service) TestSend(ctx context.Context, k TemplateKey, address string) (domain.Message, error) {
	if err := k.validate(); err != nil {
		return domain.Message{}, err
	}
	if address == "" {
		return domain.Message{}, ycerrors.Validation().Field("address", "required", "Address is required").Build()
	}
	sample := domain.Sample(k.Type)
	delete(sample, "shop_url")
	var m domain.Message
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		m, err = s.queue(ctx, tx, tenantID, request{
			Type: k.Type, Reference: "test:" + uuid.NewString(),
			To: domain.Recipient{Channel: k.Channel, Address: address, Locale: k.Locale}, Context: sample,
		})
		return err
	})
	return m, err
}

// MessagePage is a page of messages, newest first.
type MessagePage struct {
	Messages []domain.Message
	Next     *repository.MessageCursor
}

// Messages lists the shop's messages, newest first.
func (s *Service) Messages(ctx context.Context, f repository.MessageFilter, after *repository.MessageCursor, limit int) (MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var page MessagePage
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		ms, err := s.repo.ListMessages(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(ms) > limit {
			ms = ms[:limit]
			last := ms[limit-1]
			page.Next = &repository.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		for i := range ms {
			ms[i].Redact()
		}
		page.Messages = ms
		return nil
	})
	return page, err
}

// Message returns one of the shop's messages.
func (s *Service) Message(ctx context.Context, id string) (domain.Message, error) {
	var m domain.Message
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var (
			ok  bool
			err error
		)
		m, ok, err = s.repo.Message(ctx, tx, tenantID, id, false)
		if err == nil && !ok {
			err = ycerrors.NotFound("message", id)
		}
		return err
	})
	m.Redact()
	return m, err
}
