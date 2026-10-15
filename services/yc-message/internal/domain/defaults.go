package domain

import (
	"maps"
	"regexp"
)

// DefaultLocale is the locale of the platform's built-in templates.
const DefaultLocale = "en"

var localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// ValidLocale reports whether locale is a BCP 47 language tag.
func ValidLocale(locale string) bool { return localeRE.MatchString(locale) }

type templateKey struct {
	Type    Type
	Channel Channel
}

// defaults are the platform's built-in templates, in DefaultLocale. Shops
// override them per type, channel and locale.
var defaults = map[templateKey]Template{
	{TypeWelcome, ChannelEmail}: {
		Subject: "Welcome to the shop",
		Body: `Hi {{.name}},

Thanks for creating an account. You can sign in at {{.shop_url}} to track your orders and keep your cart on every device.
`,
	},
	{TypeWelcome, ChannelSMS}: {Body: `Welcome, {{.name}}! Your account is ready: {{.shop_url}}`},

	{TypePasswordReset, ChannelEmail}: {
		Subject: "Reset your password",
		Body: `Someone asked to reset the password for this account. If it was you, choose a new password here:

{{.reset_url}}

The link works once and expires at {{.expires_at}}. If you did not ask for it, ignore this email: your password stays the same.
`,
	},
	{TypePasswordReset, ChannelSMS}: {Body: `Reset your password: {{.reset_url}} (expires {{.expires_at}})`},

	{TypeChannelVerification, ChannelEmail}: {
		Subject: "Your verification code",
		Body: `Your verification code is {{.code}}.

Enter it to confirm this address. It expires in {{.expires_minutes}} minutes.
`,
	},
	{TypeChannelVerification, ChannelSMS}: {Body: `Your verification code is {{.code}}. It expires in {{.expires_minutes}} minutes.`},

	{TypeOrderConfirmation, ChannelEmail}: {
		Subject: "Order {{.order_number}} confirmed",
		Body: `Thank you for your order {{.order_number}}.

{{range .items}}{{.quantity}} x {{.name}}  {{.line_total}}
{{end}}
Items:    {{.items_total}}
{{if .requires_shipping}}Shipping: {{.shipping}}
{{end}}VAT:      {{.tax_total}}
Total:    {{.total}}

{{if .requires_shipping}}We will let you know when it ships. {{end}}See your order at {{.order_url}}
`,
	},
	{TypeOrderConfirmation, ChannelSMS}: {Body: `Order {{.order_number}} confirmed: {{.total}}. {{.order_url}}`},

	{TypeOrderShipped, ChannelEmail}: {
		Subject: "Order {{.order_number}} is on its way",
		Body: `Your order {{.order_number}} has shipped{{if .carrier}} with {{.carrier}}{{end}}.

Tracking number: {{.tracking_number}}
Track it at {{.tracking_url}}
`,
	},
	{TypeOrderShipped, ChannelSMS}: {Body: `Order {{.order_number}} has shipped. Track it: {{.tracking_url}}`},

	{TypeOrderDelivered, ChannelEmail}: {
		Subject: "Order {{.order_number}} was delivered",
		Body: `Your order {{.order_number}} was delivered. We hope you enjoy it.

If something is wrong, you can ask for a return from {{.order_url}}
`,
	},
	{TypeOrderDelivered, ChannelSMS}: {Body: `Order {{.order_number}} was delivered. {{.order_url}}`},

	{TypeOrderCancelled, ChannelEmail}: {
		Subject: "Order {{.order_number}} was cancelled",
		Body: `Your order {{.order_number}} was cancelled: {{.reason}}.
{{if .refund}}
We refunded {{.refund}} to your original payment method.
{{end}}`,
	},
	{TypeOrderCancelled, ChannelSMS}: {Body: `Order {{.order_number}} was cancelled.{{if .refund}} Refund: {{.refund}}.{{end}}`},

	{TypeRefundIssued, ChannelEmail}: {
		Subject: "Refund for order {{.order_number}}",
		Body: `We refunded {{.amount}} on order {{.order_number}} ({{.reason}}).

Refunded on this order so far: {{.total_refunded}}. It can take a few days to show on your statement.
`,
	},
	{TypeRefundIssued, ChannelSMS}: {Body: `We refunded {{.amount}} on order {{.order_number}}.`},

	{TypeCartAbandoned, ChannelEmail}: {
		Subject: "You left something in your cart",
		Body: `You still have {{.quantity}} {{if eq .quantity 1}}item{{else}}items{{end}} in your cart.

Pick up where you left off: {{.cart_url}}
`,
	},
	{TypeCartAbandoned, ChannelSMS}: {Body: `You left {{.quantity}} {{if eq .quantity 1}}item{{else}}items{{end}} in your cart: {{.cart_url}}`},

	{TypeReviewPrompt, ChannelEmail}: {
		Subject: "How was your order {{.order_number}}?",
		Body: `We would love to hear what you think of your order {{.order_number}}.

{{range .products}}{{.name}}: {{.review_url}}
{{end}}`,
	},
	{TypeReviewPrompt, ChannelSMS}: {Body: `How was order {{.order_number}}? Review it at {{.shop_url}}`},

	{TypeAdminAlert, ChannelEmail}: {
		Subject: "[{{.severity}}] {{.summary}}",
		Body: `yc-monitor raised a {{.severity}} alert at {{.opened_at}}.

{{.summary}}

Rule:      {{.rule}}
Metric:    {{.metric}}
{{range .dimensions}}{{.name}}: {{.value}}
{{end}}{{if .value}}Value:     {{.value}}
{{end}}{{if .threshold}}Threshold: {{.threshold}}
{{end}}
It resolves on its own when the condition clears, or acknowledge it in the admin console.
`,
	},
	{TypeAdminAlert, ChannelSMS}: {Body: `[{{.severity}}] {{.summary}}`},

	{TypeMilestone, ChannelEmail}: {
		Subject: "Your shop reached a milestone: {{.title}}",
		Body: `Congratulations! Your shop reached a milestone on {{.reached_at}}: {{.title}}.

See how the shop is doing in the analytics dashboard at {{.shop_url}}.
`,
	},
	{TypeMilestone, ChannelSMS}: {Body: `Congratulations! Your shop reached a milestone: {{.title}}.`},
}

// Default returns the platform's built-in template for a type and channel.
func Default(t Type, ch Channel) (Template, bool) {
	tpl, ok := defaults[templateKey{t, ch}]
	if !ok {
		return Template{}, false
	}
	tpl.Type, tpl.Channel, tpl.Locale, tpl.Platform = t, ch, DefaultLocale, true
	if tpl.Format == "" {
		tpl.Format = FormatText
	}
	return tpl, true
}

// samples are example contexts per message type: templates are checked
// against them, and test sends use them.
var samples = map[Type]map[string]any{
	TypeWelcome:             {"name": "Thandi"},
	TypePasswordReset:       {"reset_url": "https://shop.example/reset-password?token=example", "expires_at": "2026-01-01 13:00 UTC"},
	TypeChannelVerification: {"code": "123456", "expires_minutes": 15},
	TypeOrderConfirmation: {
		"order_number": "YC-000123", "order_url": "https://shop.example/orders/example",
		"items": []map[string]any{
			{"name": "Kettle", "quantity": 1, "line_total": "ZAR 299.00"},
			{"name": "Mug", "quantity": 2, "line_total": "ZAR 120.00"},
		},
		"items_total": "ZAR 419.00", "shipping": "ZAR 60.00", "tax_total": "ZAR 62.48", "total": "ZAR 479.00",
		"requires_shipping": true,
	},
	TypeOrderShipped: {
		"order_number": "YC-000123", "order_url": "https://shop.example/orders/example",
		"tracking_number": "YC1234567890AB", "carrier": "The Courier Guy",
		"tracking_url": "https://shop.example/shipping/tracking/YC1234567890AB",
	},
	TypeOrderDelivered: {"order_number": "YC-000123", "order_url": "https://shop.example/orders/example"},
	TypeOrderCancelled: {
		"order_number": "YC-000123", "order_url": "https://shop.example/orders/example",
		"reason": "Out of stock", "refund": "ZAR 479.00",
	},
	TypeRefundIssued: {
		"order_number": "YC-000123", "order_url": "https://shop.example/orders/example",
		"amount": "ZAR 299.00", "total_refunded": "ZAR 299.00", "reason": "Return accepted",
	},
	TypeCartAbandoned: {"item_count": 2, "quantity": 3, "cart_url": "https://shop.example/cart"},
	TypeReviewPrompt: {
		"order_number": "YC-000123",
		"products":     []map[string]any{{"name": "Kettle", "review_url": "https://shop.example/products/example#review"}},
	},
	TypeAdminAlert: {
		"severity": "critical", "summary": "yc-pricing failed 3 health checks", "rule": "service_unhealthy",
		"metric": "health", "dimensions": []map[string]any{{"name": "service", "value": "yc-pricing"}},
		"value": "", "threshold": "", "opened_at": "2026-01-01 13:00 UTC",
	},
	TypeMilestone: {"title": "your first order", "reached_at": "2026-01-01 13:00 UTC"},
}

// Sample returns an example context for a message type, with the fields
// every context has.
func Sample(t Type) map[string]any {
	ctx := map[string]any{"shop_url": "https://shop.example"}
	maps.Copy(ctx, samples[t])
	return ctx
}
