package http

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/slaghuis/yc-platform/services/yc-identity/api/generated"
)

func TestProfilePatchNullClears(t *testing.T) {
	var body api.ProfilePatch
	require.NoError(t, json.Unmarshal([]byte(`{"display_name":null,"phone_e164":null,"profile_image_url":null}`), &body))
	assert.True(t, body.DisplayName.IsSpecified())
	assert.True(t, body.DisplayName.IsNull())
	assert.True(t, body.PhoneE164.IsSpecified())
	assert.True(t, body.PhoneE164.IsNull())

	patch, err := profilePatch(&body)
	require.NoError(t, err)
	assert.True(t, patch.DisplayName.Set)
	assert.True(t, patch.DisplayName.Clear)
	assert.True(t, patch.PhoneE164.Set)
	assert.True(t, patch.PhoneE164.Clear)
	assert.True(t, patch.ProfileImage.Set)
	assert.True(t, patch.ProfileImage.Clear)
}
