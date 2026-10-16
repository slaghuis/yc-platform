package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlatformHostsAcceptCommaSeparatedValues(t *testing.T) {
	c := Config{PlatformHosts: []string{"Admin.Test, www.admin.test", "ops.test"}}
	assert.Equal(t, []string{"admin.test", "www.admin.test", "ops.test"}, c.platformHosts())
}
