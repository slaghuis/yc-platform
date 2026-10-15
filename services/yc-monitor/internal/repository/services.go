package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Services stores the registered services and their health. The table
// has no tenant; any transaction may use it.
type Services struct{}

const serviceColumns = `name, instance_id, base_url, COALESCE(version, ''), status, consecutive_failures,
	consecutive_successes, COALESCE(last_error, ''), last_check_at, last_healthy_at, unhealthy_since,
	registered_at, deregistered_at`

func scanService(row pgx.Row) (domain.Service, error) {
	var s domain.Service
	err := row.Scan(&s.Name, &s.InstanceID, &s.BaseURL, &s.Version, &s.Status, &s.ConsecutiveFailures,
		&s.ConsecutiveSuccesses, &s.LastError, &s.LastCheckAt, &s.LastHealthyAt, &s.UnhealthySince,
		&s.RegisteredAt, &s.DeregisteredAt)
	return s, err
}

// Register records a registration. A deregistered service is checked
// again from unknown; otherwise its health is kept.
func (Services) Register(ctx context.Context, tx pgx.Tx, name, instanceID, baseURL, version string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO services (name, instance_id, base_url, version, registered_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		ON CONFLICT (name) DO UPDATE SET
			instance_id = EXCLUDED.instance_id, base_url = EXCLUDED.base_url, version = EXCLUDED.version,
			registered_at = EXCLUDED.registered_at, deregistered_at = NULL,
			status = CASE WHEN services.status = 'deregistered' THEN 'unknown' ELSE services.status END,
			consecutive_failures = CASE WHEN services.status = 'deregistered' THEN 0 ELSE services.consecutive_failures END,
			consecutive_successes = CASE WHEN services.status = 'deregistered' THEN 0 ELSE services.consecutive_successes END
		WHERE services.registered_at <= EXCLUDED.registered_at`,
		name, instanceID, baseURL, version, at)
	if err != nil {
		return fmt.Errorf("register service: %w", err)
	}
	return nil
}

// Get returns the service, locked for update.
func (Services) Get(ctx context.Context, tx pgx.Tx, name string) (domain.Service, bool, error) {
	s, err := scanService(tx.QueryRow(ctx, `SELECT `+serviceColumns+` FROM services WHERE name = $1 FOR UPDATE`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("get service: %w", err)
	}
	return s, true, nil
}

// List returns every service by name.
func (Services) List(ctx context.Context, tx pgx.Tx) ([]domain.Service, error) {
	rows, err := tx.Query(ctx, `SELECT `+serviceColumns+` FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Service, error) { return scanService(r) })
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	return out, nil
}

// Save writes the service's health.
func (Services) Save(ctx context.Context, tx pgx.Tx, s domain.Service) error {
	_, err := tx.Exec(ctx, `
		UPDATE services SET status = $2, consecutive_failures = $3, consecutive_successes = $4,
			last_error = NULLIF($5, ''), last_check_at = $6, last_healthy_at = $7, unhealthy_since = $8,
			deregistered_at = $9
		WHERE name = $1`,
		s.Name, s.Status, s.ConsecutiveFailures, s.ConsecutiveSuccesses, s.LastError, s.LastCheckAt,
		s.LastHealthyAt, s.UnhealthySince, s.DeregisteredAt)
	if err != nil {
		return fmt.Errorf("save service: %w", err)
	}
	return nil
}
