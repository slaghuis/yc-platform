package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
)

func TestSignals(t *testing.T) {
	lv := func(onHand, reserved, threshold int64) domain.Level {
		return domain.Level{OnHand: onHand, Reserved: reserved, LowThreshold: threshold}
	}
	cases := []struct {
		name          string
		before, after domain.Level
		want          []domain.Signal
	}{
		{"unchanged", lv(5, 0, 0), lv(5, 1, 0), nil},
		{"depleted", lv(2, 0, 0), lv(2, 2, 0), []domain.Signal{domain.SignalDepleted}},
		{"restored", lv(0, 0, 0), lv(3, 0, 0), []domain.Signal{domain.SignalRestored}},
		{"low", lv(10, 0, 5), lv(10, 6, 5), []domain.Signal{domain.SignalLow}},
		{"depleted and low", lv(10, 0, 5), lv(10, 10, 5), []domain.Signal{domain.SignalDepleted, domain.SignalLow}},
		{"already low", lv(4, 0, 5), lv(4, 1, 5), nil},
		{"threshold raised past available", lv(4, 0, 0), lv(4, 0, 5), []domain.Signal{domain.SignalLow}},
		{"oversold stays depleted", lv(0, 0, 0), lv(0, 0, 0), nil},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, domain.Signals(c.before, c.after), c.name)
	}
}

func TestLevelChanges(t *testing.T) {
	l, err := domain.Level{}.Receive(10)
	require.NoError(t, err)
	_, err = l.Receive(0)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))

	l = l.Reserve(4)
	_, err = l.Adjust(-7)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeConflict), "below what is reserved")
	_, err = l.Adjust(-11)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))
	l2, err := l.Adjust(-6)
	require.NoError(t, err)
	assert.Equal(t, int64(0), l2.Available())

	c, oversold := l.Commit(4, true)
	assert.Equal(t, domain.Level{OnHand: 6, Reserved: 0}, c)
	assert.Zero(t, oversold)

	// Expired: the hold was already released; 3 of the 4 are there.
	expired := domain.Level{OnHand: 5, Reserved: 2}
	c, oversold = expired.Commit(4, false)
	assert.Equal(t, int64(1), oversold)
	assert.Equal(t, domain.Level{OnHand: 1, Reserved: 2}, c)
	assert.Equal(t, int64(-1), c.Available())

	assert.Equal(t, int64(0), domain.Level{Reserved: 1}.Release(3).Reserved)
}

func TestConfig(t *testing.T) {
	c := domain.Config{Enabled: true, DefaultTTL: 15, MinTTL: 5, MaxTTL: 30}
	require.NoError(t, c.Validate())
	assert.Equal(t, 15*time.Minute, c.TTL(nil))
	one, hundred, ten := 1, 100, 10
	assert.Equal(t, 5*time.Minute, c.TTL(&one))
	assert.Equal(t, 30*time.Minute, c.TTL(&hundred))
	assert.Equal(t, 10*time.Minute, c.TTL(&ten))
	assert.Error(t, domain.Config{DefaultTTL: 40, MinTTL: 5, MaxTTL: 30}.Validate())
	assert.Error(t, domain.Config{DefaultTTL: 1, MinTTL: 0, MaxTTL: 30}.Validate())
}

func TestNormaliseLines(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	if a > b {
		a, b = b, a
	}
	got, err := domain.NormaliseLines([]domain.Line{{SKUID: b, Quantity: 1}, {SKUID: a, Quantity: 2}, {SKUID: b, Quantity: 3}})
	require.NoError(t, err)
	assert.Equal(t, []domain.Line{{SKUID: a, Quantity: 2}, {SKUID: b, Quantity: 4}}, got)

	_, err = domain.NormaliseLines(nil)
	assert.Error(t, err)
	_, err = domain.NormaliseLines([]domain.Line{{SKUID: "x", Quantity: 0}})
	e, _ := ycerrors.As(err)
	require.NotNil(t, e)
	assert.Len(t, e.Fields, 2)
}
