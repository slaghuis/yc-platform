package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// The storefront URL contract (ADR-027 rule 12). A failure here means
// emails link to pages the storefront does not serve.
func TestShopLinksFollowTheStorefrontContract(t *testing.T) {
	l := domain.NewShopLinks("https://shop.example/")

	assert.Equal(t, "https://shop.example", l.Home())
	assert.Equal(t, "https://shop.example/orders/0b5c9a7e-1f2d-4c3b-9a8e-7d6c5b4a3f21", l.Order("0b5c9a7e-1f2d-4c3b-9a8e-7d6c5b4a3f21"))
	assert.Equal(t, "https://shop.example/cart", l.Cart())
	assert.Equal(t, "https://shop.example/products/p-1#review", l.Review("p-1"))
	assert.Equal(t, "https://shop.example/shipping/tracking/YC1234567890AB", l.Tracking("YC1234567890AB"))
}

func TestShopLinksEscapeIDs(t *testing.T) {
	l := domain.NewShopLinks("http://yc-demo.localhost:8080")

	assert.Equal(t, "http://yc-demo.localhost:8080/shipping/tracking/AB%2F12%20C", l.Tracking("AB/12 C"))
	assert.Equal(t, "http://yc-demo.localhost:8080/products/a%3Fb#review", l.Review("a?b"))
}
