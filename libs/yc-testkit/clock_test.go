package testkit_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

func TestFakeClock(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("SAST", 2*3600))
	c := testkit.NewFakeClock(start)
	assert.Equal(t, start.UTC(), c.Now())
	assert.Equal(t, time.UTC, c.Now().Location())

	c.Advance(time.Hour)
	assert.Equal(t, start.Add(time.Hour).UTC(), c.Now())

	var zero testkit.FakeClock
	assert.Equal(t, time.Unix(0, 0).UTC(), zero.Now())
	zero.Advance(time.Second)
	assert.Equal(t, time.Unix(1, 0).UTC(), zero.Now())
}
