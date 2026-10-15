package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/driver"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/repository"
)

// Dispatch sends the queued messages that are due and returns how many it
// tried.
func (s *Service) Dispatch(ctx context.Context) (int, error) {
	var due []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.DueMessages(ctx, tx, s.now().UTC(), s.cfg.BatchSize)
		return err
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.deliver(serviceContext(ctx, d.TenantID), d))
	}
	return len(due), errors.Join(errs...)
}

// deliver sends one message while holding its row, so two replicas never
// send it twice, and records the outcome: sent, retried later, or failed
// after the last attempt or a permanent refusal.
func (s *Service) deliver(ctx context.Context, d repository.Due) error {
	return s.inAnyTenant(ctx, d.TenantID, func(tx pgx.Tx, tenantID string) error {
		m, ok, err := s.repo.LockDue(ctx, tx, tenantID, d.ID, s.now().UTC())
		if err != nil || !ok {
			return err
		}
		var providerID string
		sendErr := errNoDriver
		if drv, ok := s.drivers[m.Channel]; ok {
			providerID, sendErr = drv.Send(ctx, driver.Outgoing{
				To: m.Address, Subject: m.Subject, Body: m.Body, Format: m.Format,
			})
		}
		now := s.now().UTC()
		m.Attempts++
		m.UpdatedAt = now
		switch {
		case sendErr == nil:
			m.Status, m.ProviderMessageID, m.SentAt, m.NextAttemptAt, m.LastError = domain.StatusSent, providerID, &now, nil, ""
			m.Redact()
			if err := s.repo.UpdateMessage(ctx, tx, m); err != nil {
				return err
			}
			return s.publish(ctx, tx, tenantID, events.TypeMessageSentV1, events.MessageSentV1{
				MessageID: m.ID, MessageType: string(m.Type), Channel: string(m.Channel), UserID: optional(m.UserID),
				Driver: m.Driver, SentAt: now,
			})
		case errors.Is(sendErr, driver.ErrPermanent) || errors.Is(sendErr, errNoDriver) || m.Attempts >= s.cfg.MaxAttempts:
			m.Status, m.NextAttemptAt, m.LastError = domain.StatusFailed, nil, sendErr.Error()
			m.Redact()
			telemetry.LoggerFromContext(ctx).Warn("message failed", "message_id", m.ID,
				"message_type", string(m.Type), "attempts", m.Attempts, "err", sendErr)
			if err := s.repo.UpdateMessage(ctx, tx, m); err != nil {
				return err
			}
			return s.publish(ctx, tx, tenantID, events.TypeMessageFailedV1, events.MessageFailedV1{
				MessageID: m.ID, MessageType: string(m.Type), Channel: string(m.Channel), UserID: optional(m.UserID),
				Attempts: int64(m.Attempts), LastError: sendErr.Error(), FailedAt: now,
			})
		default:
			next := now.Add(domain.Backoff(s.cfg.RetryBackoff, m.Attempts))
			m.NextAttemptAt, m.LastError = &next, sendErr.Error()
			return s.repo.UpdateMessage(ctx, tx, m)
		}
	})
}

// RunDispatcher sends due messages every interval, and as soon as a
// message is queued, until ctx ends.
func (s *Service) RunDispatcher(ctx context.Context, interval time.Duration) {
	logger := telemetry.LoggerFromContext(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.Dispatch(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("message dispatch failed", "err", err, "messages", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// DeliveryReport is a provider's report on a sent message.
type DeliveryReport struct {
	ProviderMessageID string
	Status            domain.Status // delivered or bounced
	Reason            string
}

// ReportDelivery records a delivery report from the unsigned development
// webhook, which only exists when enabled. A bounce publishes
// message.bounced.v1 so yc-preference unverifies the address. Repeating a
// report changes nothing.
func (s *Service) ReportDelivery(ctx context.Context, r DeliveryReport) error {
	if !s.cfg.FakeWebhook {
		return ycerrors.NotFound("webhook", "fake")
	}
	if r.Status != domain.StatusDelivered && r.Status != domain.StatusBounced {
		return ycerrors.Validation().Field("status", "invalid", "Status is delivered or bounced").Build()
	}
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		m, ok, err := s.repo.MessageByProviderID(ctx, tx, tenantID, r.ProviderMessageID)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("message", r.ProviderMessageID)
		}
		if m.Status == r.Status {
			return nil
		}
		if m.Status != domain.StatusSent && m.Status != domain.StatusDelivered {
			return ycerrors.Conflict("The message was not sent")
		}
		now := s.now().UTC()
		m.Status, m.UpdatedAt = r.Status, now
		if r.Status == domain.StatusDelivered {
			m.DeliveredAt = &now
			if err := s.repo.UpdateMessage(ctx, tx, m); err != nil {
				return err
			}
			return s.publish(ctx, tx, tenantID, events.TypeMessageDeliveredV1, events.MessageDeliveredV1{
				MessageID: m.ID, MessageType: string(m.Type), Channel: string(m.Channel), UserID: optional(m.UserID),
				DeliveredAt: now,
			})
		}
		reason := r.Reason
		if reason == "" {
			reason = "bounced"
		}
		m.LastError = reason
		if err := s.repo.UpdateMessage(ctx, tx, m); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeMessageBouncedV1, events.MessageBouncedV1{
			MessageID: m.ID, MessageType: string(m.Type), Channel: string(m.Channel), UserID: optional(m.UserID),
			Address: m.Address, Reason: reason, BouncedAt: now,
		})
	})
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
