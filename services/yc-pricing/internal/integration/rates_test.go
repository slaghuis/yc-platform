package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/integration"
)

func serve(t *testing.T, path, body string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFrankfurter(t *testing.T) {
	url := serve(t, "/latest", `{"amount":1.0,"base":"ZAR","date":"2026-10-14","rates":{"USD":0.05432,"IDR":1.2e3,"bad":1,"XXX":0}}`)
	p, err := integration.NewRateProvider(integration.RatesConfig{Provider: "frankfurter", FrankfurterURL: url + "/", Timeout: time.Second})
	require.NoError(t, err)
	rates, err := p.Latest(context.Background(), "ZAR")
	require.NoError(t, err)
	require.Len(t, rates, 2)
	assert.Equal(t, "0.05432", rates["USD"].String())
	assert.Equal(t, "1200", rates["IDR"].String())

	_, err = p.Latest(context.Background(), "USD")
	assert.ErrorContains(t, err, "asked for base USD")
}

func TestExchangerateHost(t *testing.T) {
	url := serve(t, "/live", `{"success":true,"source":"ZAR","quotes":{"ZARUSD":0.055,"ZAREUR":0.05}}`)
	cfg := integration.RatesConfig{Provider: "exchangerate_host", ExchangeHost: integration.ExchangeHost{URL: url}, Timeout: time.Second}
	_, err := integration.NewRateProvider(cfg)
	require.ErrorContains(t, err, "access_key")

	cfg.ExchangeHost.AccessKey = "k"
	p, err := integration.NewRateProvider(cfg)
	require.NoError(t, err)
	rates, err := p.Latest(context.Background(), "ZAR")
	require.NoError(t, err)
	assert.Equal(t, "0.055", rates["USD"].String())
	assert.Equal(t, "0.05", rates["EUR"].String())

	failing := serve(t, "/live", `{"success":false,"error":{"info":"invalid key"}}`)
	cfg.ExchangeHost.URL = failing
	p, err = integration.NewRateProvider(cfg)
	require.NoError(t, err)
	_, err = p.Latest(context.Background(), "ZAR")
	assert.ErrorContains(t, err, "invalid key")
}

func TestUnknownProvider(t *testing.T) {
	_, err := integration.NewRateProvider(integration.RatesConfig{Provider: "ecb"})
	assert.Error(t, err)
}
