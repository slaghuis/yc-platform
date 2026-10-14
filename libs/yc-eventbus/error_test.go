package eventbus_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

func TestIsPoison(t *testing.T) {
	t.Parallel()
	assert.True(t, eventbus.IsPoison(eventbus.ErrPoison))
	assert.True(t, eventbus.IsPoison(fmt.Errorf("%w: unmarshal", eventbus.ErrPoison)))
	assert.False(t, eventbus.IsPoison(errors.New("not poison")))
	assert.False(t, eventbus.IsPoison(nil))
}
