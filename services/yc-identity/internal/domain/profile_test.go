package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

func TestNormalisePhoneAndImage(t *testing.T) {
	phone, ok := domain.NormalisePhoneE164(" +27831234567 ")
	require.True(t, ok)
	assert.Equal(t, "+27831234567", phone)
	_, ok = domain.NormalisePhoneE164("not-a-phone")
	assert.False(t, ok)
	empty, ok := domain.NormalisePhoneE164("")
	require.True(t, ok)
	assert.Empty(t, empty)

	img, ok := domain.NormaliseProfileImageURL(" https://lh3.googleusercontent.com/a/x ")
	require.True(t, ok)
	assert.Equal(t, "https://lh3.googleusercontent.com/a/x", img)
	_, ok = domain.NormaliseProfileImageURL("http://insecure.example/a")
	assert.False(t, ok)
}

func TestMergeOAuthProfile(t *testing.T) {
	u := domain.User{}
	u.MergeOAuthProfile(domain.OAuthProfile{
		PhoneE164: "+27830000001", ProfileImage: "https://cdn.example/a.png",
	})
	assert.Equal(t, "+27830000001", u.PhoneE164)
	assert.Equal(t, "https://cdn.example/a.png", u.ProfileImage)

	u.MergeOAuthProfile(domain.OAuthProfile{
		PhoneE164: "+27839999999", ProfileImage: "https://cdn.example/b.png",
	})
	assert.Equal(t, "+27830000001", u.PhoneE164, "phone fills only when empty")
	assert.Equal(t, "https://cdn.example/b.png", u.ProfileImage, "image refreshes")

	u.MergeOAuthProfile(domain.OAuthProfile{})
	assert.Equal(t, "+27830000001", u.PhoneE164, "omit does not clear")
	assert.Equal(t, "https://cdn.example/b.png", u.ProfileImage)

	u.MergeOAuthProfile(domain.OAuthProfile{PhoneE164: "bad", ProfileImage: "ftp://no"})
	assert.Equal(t, "+27830000001", u.PhoneE164, "invalid skipped")
	assert.Equal(t, "https://cdn.example/b.png", u.ProfileImage)
}
