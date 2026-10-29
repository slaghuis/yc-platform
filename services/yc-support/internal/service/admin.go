package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/repository"
)

// AdminFilter narrows the shop's request list. Empty fields match any.
type AdminFilter struct {
	Status    domain.Status
	Topic     domain.Topic
	Email     string
	Reference string
}

// AdminRequests lists the shop's requests, most recently active first.
func (s *Service) AdminRequests(ctx context.Context, f AdminFilter, after *repository.Cursor, limit int) (Page[domain.Request], error) {
	rf := repository.Filter{Status: f.Status, Topic: f.Topic, Reference: f.Reference}
	if f.Status != "" && !f.Status.Valid() {
		return Page[domain.Request]{}, ycerrors.Validation().Field("status", "invalid", "Unknown status").Build()
	}
	if f.Topic != "" && !f.Topic.Valid() {
		return Page[domain.Request]{}, ycerrors.Validation().Field("topic", "invalid", "Unknown topic").Build()
	}
	if f.Email != "" {
		email, err := domain.NormaliseEmail(f.Email)
		if err != nil {
			return Page[domain.Request]{}, ycerrors.Validation().Field("email", "invalid", "Enter a valid email address").Build()
		}
		rf.Email = email
	}
	var p Page[domain.Request]
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID, _ string) error {
		rows, err := s.requests.List(ctx, tx, tenantID, rf, after, limit+1)
		p = page(rows, limit)
		return err
	})
	return p, err
}

// AdminRequest returns a request of the shop with its messages.
func (s *Service) AdminRequest(ctx context.Context, id string) (domain.Thread, error) {
	id, err := requestID(id)
	if err != nil {
		return domain.Thread{}, err
	}
	var t domain.Thread
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID, _ string) error {
		r, ok, err := s.requests.Get(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound(id)
		}
		t, err = s.thread(ctx, tx, tenantID, r)
		return err
	})
	return t, err
}

// AdminReply adds the calling shop admin's response. The requester is then
// to answer and is notified; a closed request is a conflict.
func (s *Service) AdminReply(ctx context.Context, id, body string) (domain.Message, error) {
	id, err := requestID(id)
	if err != nil {
		return domain.Message{}, err
	}
	if body, err = domain.ValidateBody(body); err != nil {
		return domain.Message{}, err
	}
	var (
		r domain.Request
		m domain.Message
	)
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID, adminID string) error {
		var ok bool
		r, ok, err = s.requests.Get(ctx, tx, tenantID, id, true)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound(id)
		}
		m, err = s.post(ctx, tx, &r, domain.AuthorShopAdmin, adminID, body)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}
	s.notifyPosted(ctx, r, m)
	return m, nil
}

// CloseRequest closes a request for good, as the calling shop admin, and
// notifies the requester and the shop. Closing a closed request is a
// conflict.
func (s *Service) CloseRequest(ctx context.Context, id string) (domain.Request, error) {
	id, err := requestID(id)
	if err != nil {
		return domain.Request{}, err
	}
	var r domain.Request
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID, adminID string) error {
		var ok bool
		r, ok, err = s.requests.Get(ctx, tx, tenantID, id, true)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFound(id)
		}
		if err := r.Close(domain.ClosedStaff, s.now().UTC()); err != nil {
			return err
		}
		if err := s.requests.Update(ctx, tx, r); err != nil {
			return err
		}
		return s.publishClosed(ctx, tx, r, adminID)
	})
	if err != nil {
		return domain.Request{}, err
	}
	s.notifyClosed(ctx, r)
	return r, nil
}

func (s *Service) publishClosed(ctx context.Context, tx pgx.Tx, r domain.Request, closedBy string) error {
	return s.publish(ctx, tx, r.TenantID, events.TypeSupportRequestClosedV1, events.SupportRequestClosedV1{
		RequestID: r.ID, Reference: r.Reference, ClosedReason: string(r.ClosedReason),
		ClosedBy: optional(closedBy), ClosedAt: *r.ClosedAt,
	})
}
