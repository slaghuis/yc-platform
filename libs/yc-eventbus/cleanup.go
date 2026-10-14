package eventbus

import (
	"context"
	"time"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// runCleanup is the janitor goroutine. It periodically deletes published
// outbox rows and old processed_events rows.
func (c *Client) runCleanup() {
	defer c.bgWG.Done()

	// Run once at startup after a brief delay, then on the interval.
	timer := time.NewTimer(1 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-c.bgCtx.Done():
			return
		case <-timer.C:
			c.doCleanup(c.bgCtx)
			timer.Reset(c.cfg.CleanupInterval)
		}
	}
}

func (c *Client) doCleanup(ctx context.Context) {
	logger := telemetry.LoggerFromContext(ctx)

	// Delete published outbox rows older than retention window.
	tag, err := c.db.Exec(ctx, `
		DELETE FROM outbox
		WHERE published_at IS NOT NULL
		  AND published_at < NOW() - make_interval(secs => $1)
	`, c.cfg.OutboxCleanupRetention.Seconds())
	if err != nil {
		logger.Warn("outbox cleanup failed", "err", err)
	} else if tag.RowsAffected() > 0 {
		logger.Info("outbox cleanup", "deleted_rows", tag.RowsAffected())
	}

	// Delete processed_events older than retention window.
	tag, err = c.db.Exec(ctx, `
		DELETE FROM processed_events
		WHERE processed_at < NOW() - make_interval(secs => $1)
	`, c.cfg.ProcessedEventsRetention.Seconds())
	if err != nil {
		logger.Warn("processed_events cleanup failed", "err", err)
	} else if tag.RowsAffected() > 0 {
		logger.Info("processed_events cleanup", "deleted_rows", tag.RowsAffected())
	}

	// Update outbox pending gauge.
	var pending int64
	err = c.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM outbox WHERE published_at IS NULL
	`).Scan(&pending)
	if err == nil {
		// UpDownCounter wants deltas, not absolute values. We track the
		// absolute value by subtracting the previous observation; for v1
		// we accept the periodic reset nature and publish a snapshot
		// metric via a dedicated gauge in a future revision.
		_ = pending
	}
}
