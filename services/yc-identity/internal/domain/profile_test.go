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
	assert.Equal(t, "https://cdn.example/a.png", u.ProfileImage, "image fills only when empty")

	u.MergeOAuthProfile(domain.OAuthProfile{})
	assert.Equal(t, "+27830000001", u.PhoneE164, "omit does not clear")
	assert.Equal(t, "https://cdn.example/a.png", u.ProfileImage)

	u.MergeOAuthProfile(domain.OAuthProfile{PhoneE164: "bad", ProfileImage: "ftp://no"})
	assert.Equal(t, "+27830000001", u.PhoneE164, "invalid skipped")
	assert.Equal(t, "https://cdn.example/a.png", u.ProfileImage)

	empty := domain.User{}
	empty.MergeOAuthProfile(domain.OAuthProfile{ProfileImage: "https://cdn.example/new.png"})
	assert.Equal(t, "https://cdn.example/new.png", empty.ProfileImage, "empty image fills from OAuth")
}

func TestNewProfilePatch(t *testing.T) {
	p, err := domain.NewProfilePatch(
		true, false, " Ann ",
		true, false, "+27831234567",
		true, false, "https://cdn.example/a.png",
	)
	require.NoError(t, err)
	u := domain.User{}
	p.Apply(&u)
	assert.Equal(t, "Ann", u.DisplayName)
	assert.Equal(t, "+27831234567", u.PhoneE164)
	assert.Equal(t, "https://cdn.example/a.png", u.ProfileImage)

	clear, err := domain.NewProfilePatch(
		true, true, "",
		true, true, "",
		true, true, "",
	)
	require.NoError(t, err)
	clear.Apply(&u)
	assert.Empty(t, u.DisplayName)
	assert.Empty(t, u.PhoneE164)
	assert.Empty(t, u.ProfileImage)

	_, err = domain.NewProfilePatch(false, false, "", false, false, "", false, false, "")
	require.Error(t, err)
	_, err = domain.NewProfilePatch(false, false, "", true, false, "", false, false, "")
	require.Error(t, err, "empty phone string")
	_, err = domain.NewProfilePatch(false, false, "", false, false, "", true, false, "http://insecure")
	require.Error(t, err)
	_, err = domain.NewProfilePatch(false, false, "", true, false, "not-e164", false, false, "")
	require.Error(t, err)

	partial, err := domain.NewProfilePatch(true, false, "Bob", false, false, "", false, false, "")
	require.NoError(t, err)
	u = domain.User{PhoneE164: "+27830000001", ProfileImage: "https://cdn.example/keep.png"}
	partial.Apply(&u)
	assert.Equal(t, "Bob", u.DisplayName)
	assert.Equal(t, "+27830000001", u.PhoneE164)
	assert.Equal(t, "https://cdn.example/keep.png", u.ProfileImage)
}
