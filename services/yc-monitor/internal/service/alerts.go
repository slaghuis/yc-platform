package service

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/repository"
)

// Page is a page of results and the cursor after it, if there are more.
type Page[T, C any] struct {
	Items []T
	Next  *C
}

// raise opens an alert, or refreshes the open one for the same rule and
// scope. Only a new alert is announced.
func (s *Service) raise(ctx context.Context, tx pgx.Tx, tenantID, rule string, dims map[string]string,
	summary string, value, threshold *float64,
) error {
	r := domain.Rules[rule]
	now := s.now().UTC()
	scope := domain.Scope(dims)
	a, ok, err := s.alerts.Active(ctx, tx, tenantID, rule, scope)
	if err != nil {
		return err
	}
	if ok {
		a.Summary, a.Value, a.Threshold, a.LastSeenAt = summary, value, threshold, now
		if r.Kind != domain.Threshold {
			a.Occurrences++
		}
		return s.alerts.Update(ctx, tx, a)
	}
	a = domain.Alert{
		ID: uuid.NewString(), TenantID: tenantID, Rule: rule, Metric: r.Metric, Scope: scope, Dimensions: dims,
		Severity: r.Severity, Summary: summary, Value: value, Threshold: threshold, Notify: r.Notify,
		Occurrences: 1, OpenedAt: now, LastSeenAt: now,
	}
	if err := s.alerts.Insert(ctx, tx, a); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.MonitorAlertOpenedV1{
		AlertID: a.ID, Rule: rule, Metric: r.Metric, Severity: string(r.Severity), Summary: summary,
		Dimensions: anyMap(dims), Value: value, Threshold: threshold, Notify: r.Notify, OpenedAt: now,
	})
}

// resolve closes the open alert for the rule and scope, if there is one.
func (s *Service) resolve(ctx context.Context, tx pgx.Tx, tenantID, rule string, dims map[string]string) error {
	a, ok, err := s.alerts.Active(ctx, tx, tenantID, rule, domain.Scope(dims))
	if err != nil || !ok {
		return err
	}
	return s.close(ctx, tx, a)
}

func (s *Service) close(ctx context.Context, tx pgx.Tx, a domain.Alert) error {
	now := s.now().UTC()
	a.ResolvedAt = &now
	if err := s.alerts.Update(ctx, tx, a); err != nil {
		return err
	}
	return s.publish(ctx, tx, a.TenantID, events.MonitorAlertResolvedV1{
		AlertID: a.ID, Rule: a.Rule, Metric: a.Metric, Severity: string(a.Severity), Summary: a.Summary,
		Dimensions: anyMap(a.Dimensions), Notify: a.Notify, OpenedAt: a.OpenedAt, ResolvedAt: now,
	})
}

// Evaluate checks the threshold rules: dead letters per minute across the
// platform and each shop's checkout success rate. Breaches open alerts;
// alerts whose condition has cleared are resolved.
func (s *Service) Evaluate(ctx context.Context) error {
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		overrides, err := s.alerts.Thresholds(ctx, tx, nil, "")
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if err := s.evaluateDLQRate(ctx, tx, s.effective(overrides, eventbus.PlatformTenantID, domain.RuleDLQRate), now); err != nil {
			return err
		}
		return s.evaluateCheckouts(ctx, tx, overrides, now)
	})
}

// RunEvaluator evaluates every interval until ctx ends.
func (s *Service) RunEvaluator(ctx context.Context, interval time.Duration) {
	every(ctx, interval, "alert evaluation", s.Evaluate)
}

// windowStart is the first 1m bucket of a window ending in now's bucket.
func windowStart(t domain.Thresholds, now time.Time) time.Time {
	return domain.Bucket1m.Start(now).Add(-t.Window() + time.Minute)
}

func (s *Service) evaluateDLQRate(ctx context.Context, tx pgx.Tx, t domain.Thresholds, now time.Time) error {
	totals, err := s.metrics.Totals(ctx, tx, []string{domain.MetricDeadLetters}, windowStart(t, now))
	if err != nil {
		return err
	}
	var n int64
	for _, m := range totals {
		n += m[domain.MetricDeadLetters]
	}
	rate := float64(n) / float64(t.WindowMinutes)
	dims := map[string]string{}
	if !t.Enabled || n < t.MinVolume || !domain.Rules[domain.RuleDLQRate].Breached(rate, t.Threshold) {
		return s.resolve(ctx, tx, eventbus.PlatformTenantID, domain.RuleDLQRate, dims)
	}
	summary := fmt.Sprintf("%d dead letters in the last %d min (%.1f/min, threshold %.1f/min)",
		n, t.WindowMinutes, rate, t.Threshold)
	return s.raise(ctx, tx, eventbus.PlatformTenantID, domain.RuleDLQRate, dims, summary, &rate, &t.Threshold)
}

func (s *Service) evaluateCheckouts(ctx context.Context, tx pgx.Tx, overrides []domain.Thresholds, now time.Time) error {
	metrics := []string{domain.MetricCheckoutsCompleted, domain.MetricCheckoutsFailed}
	widest := s.effective(overrides, eventbus.PlatformTenantID, domain.RuleCheckoutSuccessRate)
	for _, o := range overrides {
		if o.Rule == domain.RuleCheckoutSuccessRate && o.WindowMinutes > widest.WindowMinutes {
			widest.WindowMinutes = o.WindowMinutes
		}
	}
	active, err := s.metrics.Totals(ctx, tx, metrics, windowStart(widest, now))
	if err != nil {
		return err
	}
	open, err := s.alerts.ActiveByRule(ctx, tx, domain.RuleCheckoutSuccessRate)
	if err != nil {
		return err
	}
	tenants := map[string]bool{}
	for tenant := range active {
		tenants[tenant] = true
	}
	for _, a := range open {
		tenants[a.TenantID] = true
	}
	for tenant := range tenants {
		t := s.effective(overrides, tenant, domain.RuleCheckoutSuccessRate)
		totals, err := s.metrics.Totals(ctx, tx, metrics, windowStart(t, now))
		if err != nil {
			return err
		}
		completed, failed := totals[tenant][domain.MetricCheckoutsCompleted], totals[tenant][domain.MetricCheckoutsFailed]
		volume := completed + failed
		rate := 1.0
		if volume > 0 {
			rate = float64(completed) / float64(volume)
		}
		dims := map[string]string{}
		if !t.Enabled || volume < t.MinVolume || !domain.Rules[domain.RuleCheckoutSuccessRate].Breached(rate, t.Threshold) {
			if err := s.resolve(ctx, tx, tenant, domain.RuleCheckoutSuccessRate, dims); err != nil {
				return err
			}
			continue
		}
		summary := fmt.Sprintf("Checkout success rate in %s is %s over the last %d min (%d of %d; threshold %s)",
			tenant, domain.Percent(rate), t.WindowMinutes, completed, volume, domain.Percent(t.Threshold))
		if err := s.raise(ctx, tx, tenant, domain.RuleCheckoutSuccessRate, dims, summary, &rate, &t.Threshold); err != nil {
			return err
		}
	}
	return nil
}

// effective picks the shop's override, else the platform's, else the
// configured default.
func (s *Service) effective(overrides []domain.Thresholds, tenantID, rule string) domain.Thresholds {
	var platform *domain.Thresholds
	for i, o := range overrides {
		if o.Rule != rule {
			continue
		}
		if o.TenantID == tenantID {
			return o
		}
		if o.TenantID == eventbus.PlatformTenantID {
			platform = &overrides[i]
		}
	}
	if platform != nil {
		t := *platform
		t.TenantID = tenantID
		return t
	}
	t := s.cfg.Defaults[rule]
	t.TenantID, t.Rule = tenantID, rule
	return t
}

// Thresholds lists the platform-wide settings of every threshold rule and
// every shop's overrides (system admin).
func (s *Service) Thresholds(ctx context.Context) ([]domain.Thresholds, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, err
	}
	var out []domain.Thresholds
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		overrides, err := s.alerts.Thresholds(ctx, tx, nil, "")
		if err != nil {
			return err
		}
		for _, rule := range thresholdRules() {
			out = append(out, s.effective(overrides, eventbus.PlatformTenantID, rule))
		}
		for _, o := range overrides {
			if o.TenantID != eventbus.PlatformTenantID {
				out = append(out, o)
			}
		}
		return nil
	})
	return out, err
}

// ThresholdsInput changes a threshold rule. Unset fields keep their
// current values.
type ThresholdsInput struct {
	Rule          string
	TenantID      string
	Threshold     *float64
	MinVolume     *int64
	WindowMinutes *int
	Enabled       *bool
}

// PutThresholds sets a threshold rule for the platform or one shop
// (system admin). The next evaluation uses it.
func (s *Service) PutThresholds(ctx context.Context, in ThresholdsInput) (domain.Thresholds, error) {
	var t domain.Thresholds
	if err := requireSystemAdmin(ctx); err != nil {
		return t, err
	}
	r, ok := domain.Rules[in.Rule]
	v := ycerrors.Validation()
	if !ok || r.Kind != domain.Threshold {
		v.Field("rule", "invalid", "Rule is one of "+strings.Join(thresholdRules(), ", "))
	}
	if in.TenantID == "" {
		in.TenantID = eventbus.PlatformTenantID
	}
	if in.TenantID != eventbus.PlatformTenantID {
		if r.PlatformOnly {
			v.Field("tenant_id", "invalid", "This rule is platform-wide")
		} else if err := eventbus.ValidateTenantID(in.TenantID); err != nil {
			v.Field("tenant_id", "invalid", "Tenant ID is not valid")
		}
	}
	if v.HasErrors() {
		return t, v.Build()
	}
	a, _ := authctx.FromContext(ctx)
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		overrides, err := s.alerts.Thresholds(ctx, tx, nil, in.Rule)
		if err != nil {
			return err
		}
		t = s.effective(overrides, in.TenantID, in.Rule)
		t.Threshold = deref(in.Threshold, t.Threshold)
		t.MinVolume = deref(in.MinVolume, t.MinVolume)
		t.WindowMinutes = deref(in.WindowMinutes, t.WindowMinutes)
		t.Enabled = deref(in.Enabled, t.Enabled)
		if err := validThresholds(r, t); err != nil {
			return err
		}
		now := s.now().UTC()
		t.UpdatedBy, t.UpdatedAt = a.ActorID, &now
		return s.alerts.PutThresholds(ctx, tx, t)
	})
	return t, err
}

func validThresholds(r domain.Rule, t domain.Thresholds) error {
	v := ycerrors.Validation()
	if math.IsNaN(t.Threshold) || t.Threshold < 0 || (r.Below && t.Threshold > 1) {
		v.Field("threshold", "out_of_range", "Threshold is out of range")
	}
	if t.MinVolume < 0 {
		v.Field("min_volume", "out_of_range", "Minimum volume cannot be negative")
	}
	if t.WindowMinutes < 1 || t.WindowMinutes > 1440 {
		v.Field("window_minutes", "out_of_range", "Window is 1 to 1440 minutes")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

func thresholdRules() []string {
	var out []string
	for name, r := range domain.Rules {
		if r.Kind == domain.Threshold {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Alerts lists alerts newest first: every shop's and the platform's for a
// system admin (optionally one tenant's), the caller's shop's for a shop
// admin.
func (s *Service) Alerts(ctx context.Context, f domain.AlertFilter, after *repository.AlertCursor, limit int) (Page[domain.Alert, repository.AlertCursor], error) {
	var page Page[domain.Alert, repository.AlertCursor]
	err := s.readScoped(ctx, func(tx pgx.Tx, tenantID string) error {
		if tenantID != "" {
			f.TenantID = tenantID
		}
		items, err := s.alerts.List(ctx, tx, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1]
			page.Next = &repository.AlertCursor{OpenedAt: last.OpenedAt, ID: last.ID}
		}
		page.Items = items
		return nil
	})
	return page, err
}

// Acknowledge marks an alert seen. Incident alerts have no condition to
// clear, so acknowledging resolves them.
func (s *Service) Acknowledge(ctx context.Context, id string) (domain.Alert, error) {
	var a domain.Alert
	if _, err := uuid.Parse(id); err != nil {
		return a, ycerrors.NotFound("Alert", id)
	}
	actor, _ := authctx.FromContext(ctx)
	err := s.readScoped(ctx, func(tx pgx.Tx, tenantID string) error {
		var ok bool
		var err error
		a, ok, err = s.alerts.Get(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("Alert", id)
		}
		if a.AcknowledgedAt != nil || a.ResolvedAt != nil {
			return nil
		}
		now := s.now().UTC()
		a.AcknowledgedAt, a.AcknowledgedBy = &now, actor.ActorID
		if err := s.alerts.Update(ctx, tx, a); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, a.TenantID, events.MonitorAlertAcknowledgedV1{
			AlertID: a.ID, Rule: a.Rule, AcknowledgedBy: actor.ActorID, AcknowledgedAt: now,
		}); err != nil {
			return err
		}
		if domain.Rules[a.Rule].Kind != domain.Incident {
			return nil
		}
		if err := s.close(ctx, tx, a); err != nil {
			return err
		}
		a.ResolvedAt = &now
		return nil
	})
	return a, err
}

// OrderDisputed tells the shop's admins about a chargeback.
func (s *Service) OrderDisputed(ctx context.Context, env eventbus.Envelope, p events.OrderDisputedV1) error {
	if err := inShop(env.TenantID); err != nil {
		return err
	}
	summary := fmt.Sprintf("Order %s is disputed: a chargeback of %s", p.OrderNumber,
		money.New(p.Amount.AmountMinor, p.Amount.Currency))
	if p.Reason != nil && *p.Reason != "" {
		summary += " (" + strings.ReplaceAll(*p.Reason, "_", " ") + ")"
	}
	return dbkit.WithTenantTx(ctx, s.db, env.TenantID, func(tx pgx.Tx) error {
		return s.raise(ctx, tx, env.TenantID, domain.RuleOrderDisputed,
			map[string]string{"order_id": p.OrderID, "order_number": p.OrderNumber}, summary, nil, nil)
	})
}

// GDPRIncident raises a critical compliance alert for a deletion that
// missed its deadline.
func (s *Service) GDPRIncident(ctx context.Context, env eventbus.Envelope, p events.AuditGdprCascadeIncidentV1) error {
	summary := fmt.Sprintf("GDPR deletion in %s missed its deadline; not reported by %s", env.TenantID,
		strings.Join(p.MissingServices, ", "))
	return s.platformAlert(ctx, domain.RuleGDPRIncident,
		map[string]string{"tenant_id": env.TenantID, "pseudonym": p.Pseudonym}, summary)
}

// WebhookSignatureFailed raises an alert for a payment webhook that
// failed verification.
func (s *Service) WebhookSignatureFailed(ctx context.Context, env eventbus.Envelope, p events.PaymentWebhookSignatureFailedV1) error {
	summary := fmt.Sprintf("A %s webhook for %s failed signature verification: %s", p.Gateway, env.TenantID, p.Reason)
	return s.platformAlert(ctx, domain.RuleWebhookSignature,
		map[string]string{"tenant_id": env.TenantID, "gateway": p.Gateway}, summary)
}

// ExchangeRatesStale raises an alert for exchange rates that stopped
// refreshing.
func (s *Service) ExchangeRatesStale(ctx context.Context, _ eventbus.Envelope, p events.PricingExchangeRatesStaleV1) error {
	summary := fmt.Sprintf("%s exchange rates from %s are %s old", p.BaseCurrency, p.Provider,
		(time.Duration(p.StaleForSeconds) * time.Second).String())
	return s.platformAlert(ctx, domain.RuleExchangeRatesStale,
		map[string]string{"base_currency": p.BaseCurrency, "provider": p.Provider}, summary)
}

func (s *Service) platformAlert(ctx context.Context, rule string, dims map[string]string, summary string) error {
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		return s.raise(ctx, tx, eventbus.PlatformTenantID, rule, dims, summary, nil, nil)
	})
}

func inShop(tenantID string) error {
	if tenantID == eventbus.PlatformTenantID {
		return ycerrors.Validation().Field("tenant_id", "invalid", "The event belongs to a shop").Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
