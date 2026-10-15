package httpkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// HeaderIdempotencyKey carries the client's key for a retriable mutation
// (api-conventions.md §10).
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderIdempotentReplayed is set on a response replayed from the store.
const HeaderIdempotentReplayed = "Idempotent-Replayed"

const maxIdempotencyKey = 255

// IdempotencySchema creates the idempotency_keys table. Services that use
// Idempotency copy it into a migration.
//
//go:embed idempotency.sql
var IdempotencySchema string

// IdempotentResponse is a stored response.
type IdempotentResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

// IdempotencyClaim is the outcome of IdempotencyStore.Claim.
type IdempotencyClaim struct {
	// Claimed: the caller owns the key and must Complete or Release it.
	Claimed bool
	// SameRequest: an existing key was used with the same request body.
	SameRequest bool
	// Response is the stored response; nil while the first request runs.
	Response *IdempotentResponse
}

// IdempotencyStore records keys and their responses.
type IdempotencyStore interface {
	// Claim takes the key for a new request, or reports the existing one.
	// A claim older than lease (a crashed request) can be taken again.
	Claim(ctx context.Context, scope, key string, hash []byte, lease time.Duration) (IdempotencyClaim, error)
	// Complete stores the response for a claimed key.
	Complete(ctx context.Context, scope, key string, resp IdempotentResponse) error
	// Release forgets a claimed key whose request failed with a retriable
	// error, so a retry runs again.
	Release(ctx context.Context, scope, key string) error
}

// IdempotencyOptions configures Idempotency.
type IdempotencyOptions struct {
	Store IdempotencyStore
	// Required rejects POSTs without a key (400).
	Required bool
	// Lease bounds how long a running request holds its key. Default 2 min.
	Lease time.Duration
}

// Idempotency stores the response to each POST that carries an
// Idempotency-Key and replays it when the key repeats (api-conventions.md
// §10, ADR-019 rule 5). Keys are scoped to the tenant, the caller (service
// or actor) and the request path. A repeated key with a different body is
// 422; one whose first request is still running is 409. Responses of 5xx
// are not stored, so a retry runs again. Place it after authentication.
func Idempotency(opts IdempotencyOptions) func(http.Handler) http.Handler {
	if opts.Lease == 0 {
		opts.Lease = 2 * time.Minute
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(HeaderIdempotencyKey)
			if key == "" {
				if opts.Required {
					ycerrors.WriteError(w, r, ycerrors.Validation().
						Field(HeaderIdempotencyKey, "required", "Idempotency-Key header is required").Build())
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > maxIdempotencyKey {
				ycerrors.WriteError(w, r, ycerrors.Validation().
					Field(HeaderIdempotencyKey, "too_long", "Idempotency-Key is at most 255 characters").Build())
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				ycerrors.WriteError(w, r, ycerrors.Validation().Message("Unreadable request body").Wrap(err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := sha256.Sum256(body)
			scope := idempotencyScope(r)

			claim, err := opts.Store.Claim(r.Context(), scope, key, hash[:], opts.Lease)
			if err != nil {
				ycerrors.WriteError(w, r, ycerrors.Internal("idempotency claim").Wrap(err))
				return
			}
			if !claim.Claimed {
				replay(w, r, claim)
				return
			}

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			// Detached: the request's context may be what failed.
			ctx := context.WithoutCancel(r.Context())
			completed := false
			defer func(ctx context.Context) {
				if completed {
					return
				}
				// Panic or 5xx: let a retry run again.
				if err := opts.Store.Release(ctx, scope, key); err != nil {
					telemetry.LoggerFromContext(ctx).Warn("idempotency release failed", "err", err)
				}
			}(ctx)
			next.ServeHTTP(rec, r)
			if rec.status >= http.StatusInternalServerError {
				return
			}
			resp := IdempotentResponse{Status: rec.status, ContentType: rec.Header().Get("Content-Type"), Body: rec.body.Bytes()}
			if err := opts.Store.Complete(ctx, scope, key, resp); err != nil {
				telemetry.LoggerFromContext(ctx).Warn("idempotency complete failed", "err", err)
				return
			}
			completed = true
		})
	}
}

func replay(w http.ResponseWriter, r *http.Request, c IdempotencyClaim) {
	switch {
	case !c.SameRequest:
		ycerrors.WriteError(w, r, &ycerrors.Error{
			Code:    ycerrors.CodeValidationFailed,
			Status:  http.StatusUnprocessableEntity,
			Message: "Idempotency-Key was already used with a different request",
		})
	case c.Response == nil:
		ycerrors.WriteError(w, r, ycerrors.Conflict("A request with this Idempotency-Key is still in progress"))
	default:
		if c.Response.ContentType != "" {
			w.Header().Set("Content-Type", c.Response.ContentType)
		}
		w.Header().Set(HeaderIdempotentReplayed, "true")
		w.WriteHeader(c.Response.Status)
		_, _ = w.Write(c.Response.Body)
	}
}

func idempotencyScope(r *http.Request) string {
	a, _ := authctx.FromContext(r.Context())
	caller := r.Header.Get(authctx.HeaderCallerService)
	if caller == "" {
		caller = string(a.ActorType) + ":" + a.ActorID
	}
	return strings.Join([]string{a.TenantID, caller, r.Method, r.URL.Path}, "|")
}

// recorder writes through and keeps a copy of the body.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (w *recorder) WriteHeader(status int) {
	if !w.wrote {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recorder) Write(b []byte) (int, error) {
	w.wrote = true
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// PGIdempotencyStore keeps keys in the service's idempotency_keys table
// (IdempotencySchema).
type PGIdempotencyStore struct {
	DB *pgxpool.Pool
}

var _ IdempotencyStore = PGIdempotencyStore{}

// Claim implements IdempotencyStore.
func (s PGIdempotencyStore) Claim(ctx context.Context, scope, key string, hash []byte, lease time.Duration) (IdempotencyClaim, error) {
	var claimed bool
	err := s.DB.QueryRow(ctx, `
		INSERT INTO idempotency_keys (scope, key, request_hash, locked_until)
		VALUES ($1, $2, $3, NOW() + make_interval(secs => $4))
		ON CONFLICT (scope, key) DO UPDATE SET locked_until = EXCLUDED.locked_until
		 WHERE idempotency_keys.status IS NULL
		   AND idempotency_keys.locked_until < NOW()
		   AND idempotency_keys.request_hash = EXCLUDED.request_hash
		RETURNING true`, scope, key, hash, lease.Seconds()).Scan(&claimed)
	if err == nil {
		return IdempotencyClaim{Claimed: true, SameRequest: true}, nil
	}
	if !stderrors.Is(err, pgx.ErrNoRows) {
		return IdempotencyClaim{}, fmt.Errorf("claim idempotency key: %w", err)
	}
	var (
		stored      []byte
		status      *int
		contentType *string
		body        []byte
	)
	err = s.DB.QueryRow(ctx, `
		SELECT request_hash, status, content_type, body FROM idempotency_keys
		WHERE scope = $1 AND key = $2`, scope, key).Scan(&stored, &status, &contentType, &body)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Released between the two statements; let the client retry.
		return IdempotencyClaim{SameRequest: true}, nil
	}
	if err != nil {
		return IdempotencyClaim{}, fmt.Errorf("read idempotency key: %w", err)
	}
	c := IdempotencyClaim{SameRequest: bytes.Equal(stored, hash)}
	if status != nil {
		c.Response = &IdempotentResponse{Status: *status, Body: body}
		if contentType != nil {
			c.Response.ContentType = *contentType
		}
	}
	return c, nil
}

// Complete implements IdempotencyStore.
func (s PGIdempotencyStore) Complete(ctx context.Context, scope, key string, resp IdempotentResponse) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE idempotency_keys SET status = $3, content_type = $4, body = $5, completed_at = NOW()
		WHERE scope = $1 AND key = $2`, scope, key, resp.Status, resp.ContentType, resp.Body)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

// Release implements IdempotencyStore.
func (s PGIdempotencyStore) Release(ctx context.Context, scope, key string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM idempotency_keys WHERE scope = $1 AND key = $2 AND status IS NULL`, scope, key)
	if err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// Purge deletes keys older than retention (api-conventions.md §10: at
// least 24 h).
func (s PGIdempotencyStore) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < NOW() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// KeepPurged purges keys older than retention every interval until ctx
// ends.
func (s PGIdempotencyStore) KeepPurged(ctx context.Context, retention, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.Purge(ctx, retention)
			switch {
			case err != nil:
				telemetry.LoggerFromContext(ctx).Warn("idempotency purge failed", "err", err)
			case n > 0:
				telemetry.LoggerFromContext(ctx).Info("idempotency purge", "deleted_rows", n)
			}
		}
	}
}
