package service

import "testing"

func TestShopBaseURLKeepsSchemeAndPort(t *testing.T) {
	got := shopBaseURL("http://{tenant}.localhost:8070", "acme.localhost")
	if got != "http://acme.localhost:8070" {
		t.Fatalf("got %q", got)
	}
}

func TestShopBaseURLCustomDomain(t *testing.T) {
	got := shopBaseURL("https://{tenant}.example", "shop.acme.com")
	if got != "https://shop.acme.com" {
		t.Fatalf("got %q", got)
	}
}

func TestShopBaseURLFallbackWhenTemplateBroken(t *testing.T) {
	got := shopBaseURL("not-a-url", "shop.acme.com")
	if got != "https://shop.acme.com" {
		t.Fatalf("got %q", got)
	}
}
