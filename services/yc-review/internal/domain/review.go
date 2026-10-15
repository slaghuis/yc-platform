// Package domain holds yc-review's reviews, settings and product stats.
package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is a review's moderation state.
type Status string

// Review statuses.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusApproved, StatusRejected:
		return true
	}
	return false
}

// Field limits.
const (
	MinRating    = 1
	MaxRating    = 5
	MaxTitleLen  = 200
	MaxBodyLen   = 5000
	MaxReasonLen = 500
	MaxReplyLen  = 2000
)

// Reasons a review is rejected without a moderator.
const (
	ReasonNotPurchased = "unverified purchase"
	ReasonCheckExpired = "purchase could not be verified"
)

// Content is what the author writes.
type Content struct {
	Rating int
	Title  string
	Body   string
}

// Normalise trims the title and body.
func (c *Content) Normalise() {
	c.Title = strings.TrimSpace(c.Title)
	c.Body = strings.TrimSpace(c.Body)
}

// Validate checks the rating and lengths.
func (c Content) Validate() error {
	v := ycerrors.Validation()
	if c.Rating < MinRating || c.Rating > MaxRating {
		v.Field("rating", "out_of_range", "Rating is 1 to 5")
	}
	if utf8.RuneCountInString(c.Title) > MaxTitleLen {
		v.Field("title", "too_long", "Title is at most 200 characters")
	}
	if utf8.RuneCountInString(c.Body) > MaxBodyLen {
		v.Field("body", "too_long", "Body is at most 5000 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Review is a customer's review of a product.
type Review struct {
	ID        string
	ProductID string
	UserID    string
	Content
	Status           Status
	RejectionReason  string
	VerifiedPurchase bool
	PurchaseOrderID  string
	// PurchaseCheckUntil is set while the purchase is unconfirmed.
	PurchaseCheckUntil *time.Time
	NextCheckAt        *time.Time
	ShopResponse       string
	ShopResponseAt     *time.Time
	ApprovedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AwaitingPurchase reports whether yc-orders has yet to confirm the
// purchase.
func (r Review) AwaitingPurchase() bool { return r.PurchaseCheckUntil != nil }

// Approve shows the review on the product.
func (r *Review) Approve(now time.Time) {
	r.Status, r.RejectionReason, r.ApprovedAt, r.UpdatedAt = StatusApproved, "", &now, now
}

// Reject hides the review, keeping reason for the author and the audit.
func (r *Review) Reject(reason string, now time.Time) {
	r.Status, r.RejectionReason, r.UpdatedAt = StatusRejected, reason, now
	r.PurchaseCheckUntil, r.NextCheckAt = nil, nil
}

// Moderate sets the status a new or edited review gets: approved at once
// when the shop auto-approves and the purchase is settled, else pending.
func (r *Review) Moderate(mode ModerationMode, now time.Time) {
	if mode == ModerationAuto && !r.AwaitingPurchase() {
		r.Approve(now)
		return
	}
	r.Status, r.RejectionReason, r.ApprovedAt, r.UpdatedAt = StatusPending, "", nil, now
}

// ModerationMode is how a shop's reviews are approved.
type ModerationMode string

// Moderation modes.
const (
	ModerationManual ModerationMode = "manual"
	ModerationAuto   ModerationMode = "auto"
)

// Settings are a shop's review rules.
type Settings struct {
	ModerationMode  ModerationMode
	RequirePurchase bool
	AllowEdit       bool
	UpdatedAt       time.Time
}

// Validate checks the moderation mode.
func (s Settings) Validate() error {
	switch s.ModerationMode {
	case ModerationManual, ModerationAuto:
		return nil
	}
	return ycerrors.Validation().Field("moderation_mode", "invalid", "Moderation mode is manual or auto").Build()
}

// Stats are a product's approved-review and like counts. Version grows
// with every change.
type Stats struct {
	ProductID   string
	RatingCount int64
	RatingSum   int64
	LikeCount   int64
	Version     int64
}

// Average is the mean approved rating, 0 without reviews.
func (s Stats) Average() float64 {
	if s.RatingCount == 0 {
		return 0
	}
	return float64(s.RatingSum) / float64(s.RatingCount)
}

// ValidReason checks a moderator's rejection reason.
func ValidReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReasonLen {
		return "", ycerrors.Validation().Field("reason", "invalid", "Reason is 1 to 500 characters").Build()
	}
	return reason, nil
}

// ValidResponse checks a shop's response to a review.
func ValidResponse(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > MaxReplyLen {
		return "", ycerrors.Validation().Field("response", "invalid", "Response is 1 to 2000 characters").Build()
	}
	return text, nil
}
