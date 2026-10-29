package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/repository"
)

// Create opens a support request with its first message, as the calling
// customer or visitor. Visitors are identified by the contact email alone.
func (s *Service) Create(ctx context.Context, in domain.NewRequest) (domain.Thread, error) {
	a, _ := authctx.FromContext(ctx)
	userType, author, userID := domain.UserVisitor, domain.AuthorVisitor, ""
	if a.ActorType == authctx.ActorCustomer && a.ActorID != "" {
		userType, author, userID = domain.UserCustomer, domain.AuthorCustomer, a.ActorID
	} else if a.ActorType != authctx.ActorVisitor {
		return domain.Thread{}, ycerrors.Forbidden("Only customers and visitors open support requests").
			WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Thread{}, err
	}
	if err := in.Validate(); err != nil {
		return domain.Thread{}, err
	}
	if in.OrderID != "" {
		id, err := uuid.Parse(in.OrderID)
		if err != nil {
			return domain.Thread{}, ycerrors.Validation().Field("order_id", "invalid", "Order ID is not valid").Build()
		}
		in.OrderID = id.String()
	}

	now := s.now().UTC()
	var t domain.Thread
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx, tenantID string) error {
		r := domain.Request{
			ID: uuid.NewString(), TenantID: tenantID, Topic: in.Topic, Status: domain.StatusNew,
			ContactEmail: in.ContactEmail, ContactName: in.ContactName, ContactPhone: in.ContactPhone,
			UserID: userID, UserType: userType, OrderID: in.OrderID, PageURL: in.PageURL,
			LastActivityAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.insertRequest(ctx, tx, &r); err != nil {
			return err
		}
		m := domain.Message{
			ID: uuid.NewString(), RequestID: r.ID, AuthorType: author, AuthorUserID: userID,
			Body: in.Body, CreatedAt: now,
		}
		if err := s.messages.Insert(ctx, tx, tenantID, m); err != nil {
			return err
		}
		evt := events.SupportRequestCreatedV1{
			RequestID: r.ID, Reference: r.Reference, Topic: string(r.Topic), UserType: string(r.UserType),
			UserID: optional(r.UserID), OrderID: optional(r.OrderID), CreatedAt: now,
		}
		if err := s.publish(ctx, tx, tenantID, events.TypeSupportRequestCreatedV1, evt); err != nil {
			return err
		}
		t = domain.Thread{Request: r, Messages: []domain.Message{m}}
		return nil
	})
	if err != nil {
		return domain.Thread{}, err
	}
	s.notifyCreated(ctx, t.Request, t.Messages[0])
	return t, nil
}

// insertRequest stores r under a fresh reference, trying again when the
// random one is already used in the shop.
func (s *Service) insertRequest(ctx context.Context, tx pgx.Tx, r *domain.Request) error {
	for range maxReferenceAttempts {
		ref, err := domain.NewReference(nil)
		if err != nil {
			return ycerrors.Internal("generate support reference: " + err.Error())
		}
		r.Reference = ref
		err = s.requests.Insert(ctx, tx, *r)
		if !errors.Is(err, repository.ErrReferenceTaken) {
			return err
		}
	}
	return ycerrors.Internal("no free support reference after retries")
}

// MyRequests lists the calling customer's requests, most recently active
// first, including ones claimed from visitor submissions.
func (s *Service) MyRequests(ctx context.Context, after *repository.Cursor, limit int) (Page[domain.Request], error) {
	var p Page[domain.Request]
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		rows, err := s.requests.List(ctx, tx, tenantID, repository.Filter{UserID: userID}, after, limit+1)
		p = page(rows, limit)
		return err
	})
	return p, err
}

// MyRequest returns one of the calling customer's requests with its
// messages. Another customer's request is not found.
func (s *Service) MyRequest(ctx context.Context, id string) (domain.Thread, error) {
	id, err := requestID(id)
	if err != nil {
		return domain.Thread{}, err
	}
	var t domain.Thread
	err = s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		r, ok, err := s.requests.Get(ctx, tx, tenantID, id, false)
		if err != nil {
			return err
		}
		if !ok || !r.OwnedBy(userID) {
			return errNotFound(id)
		}
		t, err = s.thread(ctx, tx, tenantID, r)
		return err
	})
	return t, err
}

// Reply adds the calling customer's message to their request. The shop is
// then to answer; a closed request is a conflict.
func (s *Service) Reply(ctx context.Context, id, body string) (domain.Message, error) {
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
	err = s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		var ok bool
		r, ok, err = s.requests.Get(ctx, tx, tenantID, id, true)
		if err != nil {
			return err
		}
		if !ok || !r.OwnedBy(userID) {
			return errNotFound(id)
		}
		m, err = s.post(ctx, tx, &r, domain.AuthorCustomer, userID, body)
		return err
	})
	if err != nil {
		return domain.Message{}, err
	}
	s.notifyPosted(ctx, r, m)
	return m, nil
}

// post adds a message to the locked request r and moves its status. A
// closed request is a conflict.
func (s *Service) post(ctx context.Context, tx pgx.Tx, r *domain.Request, author domain.AuthorType, authorUserID, body string) (domain.Message, error) {
	now := s.now().UTC()
	if err := r.Post(author, now); err != nil {
		return domain.Message{}, err
	}
	m := domain.Message{
		ID: uuid.NewString(), RequestID: r.ID, AuthorType: author, AuthorUserID: authorUserID,
		Body: body, CreatedAt: now,
	}
	if err := s.messages.Insert(ctx, tx, r.TenantID, m); err != nil {
		return domain.Message{}, err
	}
	if err := s.requests.Update(ctx, tx, *r); err != nil {
		return domain.Message{}, err
	}
	return m, s.publishPosted(ctx, tx, *r, m)
}

func (s *Service) publishPosted(ctx context.Context, tx pgx.Tx, r domain.Request, m domain.Message) error {
	return s.publish(ctx, tx, r.TenantID, events.TypeSupportMessagePostedV1, events.SupportMessagePostedV1{
		RequestID: r.ID, MessageID: m.ID, Reference: r.Reference, AuthorType: string(m.AuthorType),
		AuthorUserID: optional(m.AuthorUserID), Status: string(r.Status), PostedAt: m.CreatedAt,
	})
}

// thread loads r's messages.
func (s *Service) thread(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Request) (domain.Thread, error) {
	msgs, err := s.messages.List(ctx, tx, tenantID, r.ID)
	return domain.Thread{Request: r, Messages: msgs}, err
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
