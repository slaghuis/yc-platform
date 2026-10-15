package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
)

func TestContentValidate(t *testing.T) {
	c := domain.Content{Rating: 5, Title: "  Great mug ", Body: " Holds tea. "}
	c.Normalise()
	assert.Equal(t, "Great mug", c.Title)
	assert.Equal(t, "Holds tea.", c.Body)
	require.NoError(t, c.Validate())

	for _, bad := range []domain.Content{
		{Rating: 0},
		{Rating: 6},
		{Rating: 3, Title: strings.Repeat("x", domain.MaxTitleLen+1)},
		{Rating: 3, Body: strings.Repeat("x", domain.MaxBodyLen+1)},
	} {
		err := bad.Validate()
		assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed), "%+v", bad)
	}
	assert.NoError(t, domain.Content{Rating: 1, Title: strings.Repeat("é", domain.MaxTitleLen)}.Validate(), "limits count characters")
}

func TestModerate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	var r domain.Review
	r.Moderate(domain.ModerationManual, now)
	assert.Equal(t, domain.StatusPending, r.Status)

	r.Moderate(domain.ModerationAuto, now)
	assert.Equal(t, domain.StatusApproved, r.Status)
	require.NotNil(t, r.ApprovedAt)

	until := now.Add(time.Hour)
	r.PurchaseCheckUntil = &until
	r.Moderate(domain.ModerationAuto, now)
	assert.Equal(t, domain.StatusPending, r.Status, "an unconfirmed purchase waits even in auto mode")
	assert.Nil(t, r.ApprovedAt)

	r.Reject(domain.ReasonCheckExpired, now)
	assert.Equal(t, domain.StatusRejected, r.Status)
	assert.False(t, r.AwaitingPurchase(), "rejection ends the purchase check")

	r.Approve(now)
	assert.Empty(t, r.RejectionReason)
}

func TestSettingsAndStats(t *testing.T) {
	assert.NoError(t, domain.Settings{ModerationMode: domain.ModerationAuto}.Validate())
	assert.Error(t, domain.Settings{ModerationMode: "sometimes"}.Validate())

	assert.Zero(t, domain.Stats{}.Average())
	assert.InDelta(t, 4.5, domain.Stats{RatingCount: 2, RatingSum: 9}.Average(), 1e-9)
}

func TestReasonAndResponse(t *testing.T) {
	r, err := domain.ValidReason("  spam ")
	require.NoError(t, err)
	assert.Equal(t, "spam", r)
	_, err = domain.ValidReason("   ")
	assert.Error(t, err)

	_, err = domain.ValidResponse(strings.Repeat("x", domain.MaxReplyLen+1))
	assert.Error(t, err)
}
