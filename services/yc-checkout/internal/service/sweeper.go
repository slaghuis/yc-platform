package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/repository"
)

// sweepBatch bounds the sessions one sweep acts on.
const sweepBatch = 100

// ReleaseOwnLeases frees the sessions an earlier run of this replica was
// driving when it stopped, so resumption need not wait for the leases to
// lapse.
func (s *Service) ReleaseOwnLeases(ctx context.Context) (int64, error) {
	var n int64
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		n, err = s.repo.ReleaseLeases(ctx, tx, s.cfg.Host+"/", s.runID)
		return err
	})
	return n, err
}

// Sweep expires sessions past their TTL (ADR-011) and resumes sessions
// whose next attempt is due, including those a stopped replica left
// mid-saga. It returns how many sessions it acted on.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	var expired, resumable []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		now := s.now().UTC()
		var err error
		if expired, err = s.repo.ExpiredSessions(ctx, tx, now, sweepBatch); err != nil {
			return err
		}
		resumable, err = s.repo.ResumableSessions(ctx, tx, now, sweepBatch)
		return err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, d := range expired {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.Expire(ctx, d.TenantID, d.ID)
		errs = append(errs, err)
		if ok {
			n++
		}
	}
	for _, d := range resumable {
		if ctx.Err() != nil {
			break
		}
		if _, err := s.Resume(ctx, d.TenantID, d.ID); err != nil {
			errs = append(errs, err)
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Expire ends a session past its TTL that has not reached the point of no
// return: a quote just expires; a running saga is compensated first.
func (s *Service) Expire(ctx context.Context, tenantID, id string) (bool, error) {
	ctx = serviceContext(ctx, tenantID)
	token, ok, err := s.acquireAny(ctx, tenantID, id)
	if err != nil || !ok {
		return false, err
	}
	expired := false
	err = s.save(ctx, tenantID, token, id, func(tx pgx.Tx, sess *domain.Session) error {
		now := s.now().UTC()
		if !sess.Expired(now) || !sess.State.Compensable() {
			return nil
		}
		expired = true
		if sess.State != domain.StatePricing {
			return s.beginCompensation(ctx, tx, tenantID, sess, domain.ReasonSessionExpired, nil, domain.StateExpired)
		}
		if err := sess.Compensate(domain.ReasonSessionExpired, nil, domain.StateExpired, now); err != nil {
			return err
		}
		sess.Finish(now)
		return s.ended(ctx, tx, tenantID, *sess, domain.StatePricing)
	})
	if err != nil || !expired {
		s.release(context.WithoutCancel(ctx), tenantID, id, token)
		return false, err
	}
	_, err = s.run(ctx, tenantID, id, token)
	return true, err
}

// acquireAny takes the lease of a session in any state, for the sweeper
// to expire it.
func (s *Service) acquireAny(ctx context.Context, tenantID, id string) (string, bool, error) {
	token := s.newToken()
	acquired := false
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		sess, ok, err := s.repo.Session(ctx, tx, tenantID, id, true)
		now := s.now().UTC()
		if err != nil || !ok || sess.Leased(now) {
			return err
		}
		until := now.Add(s.cfg.LeaseTTL)
		sess.LeaseOwner, sess.LeaseUntil = token, &until
		acquired = true
		return s.repo.UpdateSession(ctx, tx, tenantID, sess)
	})
	return token, acquired, err
}

// Resume drives a session in the background, as yc-checkout.
func (s *Service) Resume(ctx context.Context, tenantID, id string) (domain.Session, error) {
	return s.run(serviceContext(ctx, tenantID), tenantID, id, "")
}

// run drives a session in the background, bounded like a confirm request.
func (s *Service) run(ctx context.Context, tenantID, id, token string) (domain.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ConfirmDeadline)
	defer cancel()
	return s.drive(ctx, tenantID, id, token)
}

// RunSweeper resumes interrupted sessions at once, then sweeps every
// interval until ctx ends.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	logger := telemetry.LoggerFromContext(ctx)
	if n, err := s.ReleaseOwnLeases(ctx); err != nil {
		logger.Warn("release leases from the previous run", "err", err)
	} else if n > 0 {
		logger.Info("resuming checkout sessions from the previous run", "sessions", n)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := s.Sweep(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("checkout sweep failed", "err", err, "sessions", n)
		case n > 0:
			logger.Info("checkout sweep", "sessions", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReservationExpired compensates a session whose stock hold lapsed before
// payment succeeded (ADR-019 rule 9). A session someone is driving is
// retried later (the event is redelivered).
func (s *Service) ReservationExpired(ctx context.Context, tenantID, sessionID, reservationID string) error {
	ctx = serviceContext(ctx, tenantID)
	sess, err := s.load(ctx, tenantID, sessionID)
	if err != nil {
		return ignoreNotFound(err)
	}
	if sess.ReservationID != reservationID || !sess.State.Compensable() || sess.State == domain.StatePricing {
		return nil
	}
	token, ok, err := s.acquire(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return errSessionBusy
	}
	if _, err := s.startCompensation(ctx, tenantID, token, sessionID, domain.ReasonReservationExpired,
		map[string]any{"reservation_id": reservationID}, domain.StateFailed); err != nil {
		s.release(context.WithoutCancel(ctx), tenantID, sessionID, token)
		return err
	}
	_, err = s.run(ctx, tenantID, sessionID, token)
	return err
}

// errSessionBusy: another driver holds the session; try again later.
var errSessionBusy = errors.New("checkout: session is being driven")

// PaymentSettled resumes a session waiting on a payment that settled
// asynchronously (3-D Secure, delayed confirmation; ADR-019 rule 8).
func (s *Service) PaymentSettled(ctx context.Context, tenantID, sessionID string, waitingIn domain.State) error {
	ctx = serviceContext(ctx, tenantID)
	sess, err := s.load(ctx, tenantID, sessionID)
	if err != nil {
		return ignoreNotFound(err)
	}
	if sess.State != waitingIn {
		return nil
	}
	_, err = s.run(ctx, tenantID, sessionID, "")
	return err
}

func ignoreNotFound(err error) error {
	if isNotFound(err) {
		return nil
	}
	return err
}
