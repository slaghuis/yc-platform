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

{{range .items}}• {{.name}} (×{{.quantity}})
{{if .image_url}}  {{.image_url}}
{{end}}{{end}}
Pick up where you left off: {{.cart_url}}
`,
	},
	{TypeCartAbandoned, ChannelSMS}: {Body: `You left {{.quantity}} {{if eq .quantity 1}}item{{else}}items{{end}} in your cart: {{.cart_url}}`},

	{TypeCartAbandonedFollowup, ChannelEmail}: {
		Subject: "Your cart is still waiting",
		Body: `Just a reminder — the items in your cart are still waiting for you.

{{range .items}}• {{.name}} (×{{.quantity}})
{{end}}
Continue checkout: {{.cart_url}}
`,
	},
	{TypeCartAbandonedFollowup, ChannelSMS}: {Body: `Your cart is still waiting ({{.quantity}} {{if eq .quantity 1}}item{{else}}items{{end}}): {{.cart_url}}`},

	{TypeCartAbandonedFinal, ChannelEmail}: {
		Subject: "Last reminder: items in your cart",
		Body: `This is a final reminder that you still have items in your cart.

{{range .items}}• {{.name}} (×{{.quantity}})
{{if .image_url}}  {{.image_url}}
{{end}}{{end}}
Complete your order: {{.cart_url}}
`,
	},
	{TypeCartAbandonedFinal, ChannelSMS}: {Body: `Last reminder: {{.quantity}} {{if eq .quantity 1}}item{{else}}items{{end}} still in your cart: {{.cart_url}}`},

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

	{TypeAdminInvite, ChannelEmail}: {
		Subject: "You're invited as {{.role}}",
		Body: `You have been invited to join as {{.role}}.

Accept the invitation here:

{{.invite_url}}

The link works once and expires at {{.expires_at}}. If you did not expect this, ignore this email.
`,
	},
	{TypeAdminInvite, ChannelSMS}: {Body: `You're invited as {{.role}}: {{.invite_url}} (expires {{.expires_at}})`},

	{TypeSupportAck, ChannelEmail}: {
		Subject: "We received your request {{.reference}}",
		Body: `Thanks {{.contact_name}}. We received your support request about {{.topic}}.

Your reference is {{.reference}}.

{{if .support_url}}View the conversation: {{.support_url}}

{{end}}We will reply as soon as we can.
`,
	},
	{TypeSupportAck, ChannelSMS}: {Body: `We received your request {{.reference}} ({{.topic}}).`},

	{TypeSupportShopReply, ChannelEmail}: {
		Subject: "Update on your request {{.reference}}",
		Body: `The shop replied to your support request {{.reference}} ({{.topic}}).

{{if .excerpt}}{{.excerpt}}

{{end}}{{if .support_url}}Continue the conversation: {{.support_url}}
{{else}}Reply is not available in the app for guest requests; register with this email to continue in your account, or wait for a further email.
{{end}}`,
	},
	{TypeSupportShopReply, ChannelSMS}: {Body: `Update on request {{.reference}}: the shop replied.{{if .support_url}} {{.support_url}}{{end}}`},

	{TypeSupportClosed, ChannelEmail}: {
		Subject: "Request {{.reference}} is closed",
		Body: `Support request {{.reference}} ({{.topic}}) is now closed{{if eq .closed_reason "idle"}} after 72 hours without a reply{{end}}.

{{if and (eq .audience "customer") .support_url}}View the thread: {{.support_url}}
{{end}}If you still need help, open a new support request.
`,
	},
	{TypeSupportClosed, ChannelSMS}: {Body: `Request {{.reference}} is closed.{{if eq .closed_reason "idle"}} (no reply for 72h){{end}}`},

	{TypeSupportNewRequest, ChannelEmail}: {
		Subject: "New support request {{.reference}}",
		Body: `A new support request was opened.

Reference: {{.reference}}
Topic:     {{.topic}}
From:      {{.contact_name}} <{{.contact_email}}>

{{if .excerpt}}{{.excerpt}}
{{end}}
Open it in the shop admin console under Support.
`,
	},
	{TypeSupportNewRequest, ChannelSMS}: {Body: `New support request {{.reference}} ({{.topic}}) from {{.contact_email}}.`},

	{TypeSupportCustomerReply, ChannelEmail}: {
		Subject: "Reply on support request {{.reference}}",
		Body: `The customer replied on support request {{.reference}} ({{.topic}}).

From: {{.contact_name}} <{{.contact_email}}>

{{if .excerpt}}{{.excerpt}}
{{end}}
Open it in the shop admin console under Support.
`,
	},
	{TypeSupportCustomerReply, ChannelSMS}: {Body: `Customer reply on {{.reference}} ({{.topic}}).`},
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
		"delivery_slot_id":  "sched_1", "delivery_slot_start": "2026-11-03T08:00:00Z",
		"delivery_slot_end": "2026-11-03T08:30:00Z", "delivery_slot_timezone": "Africa/Johannesburg",
		"delivery_slot_label": "3 Nov 2026, 10:00–10:30",
	},
	TypeOrderShipped: {
		"order_number": "YC-000123", "order_url": "https://shop.example/orders/example",
		"tracking_number": "YC1234567890AB", "carrier": "The Courier Guy",
		"tracking_url":        "https://shop.example/shipping/tracking/YC1234567890AB",
		"delivery_slot_label": "3 Nov 2026, 10:00–10:30",
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
	TypeCartAbandoned: {
		"item_count": 2, "quantity": 3, "cart_url": "https://shop.example/cart?recover=1",
		"items": []map[string]any{
			{"name": "Kettle", "quantity": 1, "image_url": "https://images.example/kettle.jpg"},
			{"name": "Mug", "quantity": 2, "image_url": ""},
		},
	},
	TypeCartAbandonedFollowup: {
		"item_count": 2, "quantity": 3, "cart_url": "https://shop.example/cart?recover=2",
		"items": []map[string]any{
			{"name": "Kettle", "quantity": 1},
			{"name": "Mug", "quantity": 2},
		},
	},
	TypeCartAbandonedFinal: {
		"item_count": 2, "quantity": 3, "cart_url": "https://shop.example/cart?recover=3",
		"items": []map[string]any{
			{"name": "Kettle", "quantity": 1, "image_url": "https://images.example/kettle.jpg"},
		},
	},
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
	TypeAdminInvite: {
		"role": "shop_admin", "invite_url": "https://shop.example/account/accept-invite?token=example",
		"expires_at": "2026-01-01 13:00 UTC",
	},
	TypeSupportAck: {
		"reference": "SUP-ABC123", "topic": "delivery", "contact_name": "Thandi",
		"support_url": "https://shop.example/account/support/example",
	},
	TypeSupportShopReply: {
		"reference": "SUP-ABC123", "topic": "delivery", "excerpt": "Your order is on the way.",
		"support_url": "https://shop.example/account/support/example",
	},
	TypeSupportClosed: {
		"reference": "SUP-ABC123", "topic": "delivery", "audience": "customer", "closed_reason": "idle",
		"support_url": "https://shop.example/account/support/example",
	},
	TypeSupportNewRequest: {
		"reference": "SUP-ABC123", "topic": "delivery", "contact_name": "Thandi",
		"contact_email": "thandi@example.com", "excerpt": "Where is my order?",
	},
	TypeSupportCustomerReply: {
		"reference": "SUP-ABC123", "topic": "delivery", "contact_name": "Thandi",
		"contact_email": "thandi@example.com", "excerpt": "Thanks, still waiting.",
	},
}

// Sample returns an example context for a message type, with the fields
// every context has.
func Sample(t Type) map[string]any {
	ctx := map[string]any{"shop_url": "https://shop.example"}
	maps.Copy(ctx, samples[t])
	return ctx
}
