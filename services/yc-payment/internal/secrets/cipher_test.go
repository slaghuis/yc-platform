package secrets_test

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/secrets"
)

func key(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

func TestCipher(t *testing.T) {
	c, err := secrets.NewCipher(key(t))
	require.NoError(t, err)
	blob, err := c.Seal([]byte(`{"api_key":"sk_test_x"}`), []byte("shop-a|stripe"))
	require.NoError(t, err)
	assert.NotContains(t, string(blob), "sk_test_x")

	out, err := c.Open(blob, []byte("shop-a|stripe"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"api_key":"sk_test_x"}`, string(out))

	_, err = c.Open(blob, []byte("shop-b|stripe"))
	assert.ErrorIs(t, err, secrets.ErrOpen, "bound to its row")
	other, err := secrets.NewCipher(key(t))
	require.NoError(t, err)
	_, err = other.Open(blob, []byte("shop-a|stripe"))
	assert.ErrorIs(t, err, secrets.ErrOpen)
	_, err = c.Open(blob[:5], nil)
	assert.ErrorIs(t, err, secrets.ErrOpen)

	_, err = secrets.NewCipher("short")
	assert.Error(t, err)
	_, err = secrets.NewCipher(base64.StdEncoding.EncodeToString([]byte("sixteen byte key")))
	assert.Error(t, err)
}
