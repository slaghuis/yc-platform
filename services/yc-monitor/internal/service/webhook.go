package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// WebhookAlert is the body posted to the critical webhook. Text makes it
// a valid Slack incoming-webhook message.
type WebhookAlert struct {
	Text       string         `json:"text"`
	AlertID    string         `json:"alert_id"`
	TenantID   string         `json:"tenant_id"`
	Rule       string         `json:"rule"`
	Severity   string         `json:"severity"`
	Summary    string         `json:"summary"`
	Dimensions map[string]any `json:"dimensions,omitempty"`
	OpenedAt   time.Time      `json:"opened_at"`
}

// CriticalWebhook posts a critical alert to the configured webhook. A
// failed post is retried by the event bus.
func (s *Service) CriticalWebhook(ctx context.Context, env eventbus.Envelope, p events.MonitorAlertOpenedV1) error {
	if s.cfg.CriticalWebhookURL == "" || p.Severity != string(domain.SeverityCritical) {
		return nil
	}
	body, err := json.Marshal(WebhookAlert{
		Text:    fmt.Sprintf("[%s] %s", p.Severity, p.Summary),
		AlertID: p.AlertID, TenantID: env.TenantID, Rule: p.Rule, Severity: p.Severity, Summary: p.Summary,
		Dimensions: p.Dimensions, OpenedAt: p.OpenedAt,
	})
	if err != nil {
		return fmt.Errorf("encode webhook alert: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.CriticalWebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("critical webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("critical webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("critical webhook returned %d", resp.StatusCode)
	}
	return nil
}
