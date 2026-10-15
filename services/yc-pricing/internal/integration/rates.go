// Package integration holds yc-pricing's clients for external services:
// the exchange rate providers (docs/services/yc-pricing.md, Exchange Rates).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Provider names, as in pricing.exchange_rates_updated.v1.
const (
	ProviderFrankfurter      = "frankfurter"
	ProviderExchangerateHost = "exchangerate_host"
)

// RateProvider fetches the latest rates from base: units of each quote
// currency per one unit of base.
type RateProvider interface {
	Name() string
	Latest(ctx context.Context, base string) (map[string]money.Rate, error)
}

// RatesConfig is the `rates:` block.
type RatesConfig struct {
	Provider       string        `koanf:"provider"`
	FrankfurterURL string        `koanf:"frankfurter_url"`
	ExchangeHost   ExchangeHost  `koanf:"exchangerate_host"`
	Timeout        time.Duration `koanf:"timeout"`
}

// ExchangeHost configures exchangerate.host, which needs an access key.
type ExchangeHost struct {
	URL       string `koanf:"url"`
	AccessKey string `koanf:"access_key"`
}

// NewRateProvider returns the configured provider.
func NewRateProvider(cfg RatesConfig) (RateProvider, error) {
	client := &http.Client{Timeout: cfg.Timeout}
	switch cfg.Provider {
	case ProviderFrankfurter:
		return Frankfurter{BaseURL: strings.TrimSuffix(cfg.FrankfurterURL, "/"), Client: client}, nil
	case ProviderExchangerateHost:
		if cfg.ExchangeHost.AccessKey == "" {
			return nil, errors.New("rates.exchangerate_host.access_key is required for provider exchangerate_host")
		}
		return ExchangerateHost{BaseURL: strings.TrimSuffix(cfg.ExchangeHost.URL, "/"), AccessKey: cfg.ExchangeHost.AccessKey, Client: client}, nil
	default:
		return nil, fmt.Errorf("rates.provider %q: want %s or %s", cfg.Provider, ProviderFrankfurter, ProviderExchangerateHost)
	}
}

// Frankfurter is the ECB reference rate API (no key; updated daily).
type Frankfurter struct {
	BaseURL string // e.g. https://api.frankfurter.dev/v1
	Client  *http.Client
}

// Name implements RateProvider.
func (Frankfurter) Name() string { return ProviderFrankfurter }

// Latest implements RateProvider.
func (f Frankfurter) Latest(ctx context.Context, base string) (map[string]money.Rate, error) {
	var body struct {
		Base  string                 `json:"base"`
		Rates map[string]json.Number `json:"rates"`
	}
	if err := getJSON(ctx, f.Client, f.BaseURL+"/latest?base="+url.QueryEscape(base), &body); err != nil {
		return nil, fmt.Errorf("frankfurter: %w", err)
	}
	if body.Base != base {
		return nil, fmt.Errorf("frankfurter: asked for base %s, got %q", base, body.Base)
	}
	return parseRates(body.Rates, func(k string) string { return k })
}

// ExchangerateHost is the exchangerate.host API.
type ExchangerateHost struct {
	BaseURL   string // e.g. https://api.exchangerate.host
	AccessKey string
	Client    *http.Client
}

// Name implements RateProvider.
func (ExchangerateHost) Name() string { return ProviderExchangerateHost }

// Latest implements RateProvider.
func (e ExchangerateHost) Latest(ctx context.Context, base string) (map[string]money.Rate, error) {
	var body struct {
		Success bool                   `json:"success"`
		Source  string                 `json:"source"`
		Quotes  map[string]json.Number `json:"quotes"`
		Error   struct {
			Info string `json:"info"`
		} `json:"error"`
	}
	q := url.Values{"access_key": {e.AccessKey}, "source": {base}}
	if err := getJSON(ctx, e.Client, e.BaseURL+"/live?"+q.Encode(), &body); err != nil {
		return nil, fmt.Errorf("exchangerate.host: %w", err)
	}
	if !body.Success {
		return nil, fmt.Errorf("exchangerate.host: %s", body.Error.Info)
	}
	if body.Source != base {
		return nil, fmt.Errorf("exchangerate.host: asked for source %s, got %q", base, body.Source)
	}
	return parseRates(body.Quotes, func(k string) string { return strings.TrimPrefix(k, base) })
}

func getJSON(ctx context.Context, client *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	return dec.Decode(out)
}

// parseRates keeps positive rates keyed by valid currency codes. Numbers
// may use exponent notation, so they are parsed as exact rationals.
func parseRates(in map[string]json.Number, quote func(string) string) (map[string]money.Rate, error) {
	out := make(map[string]money.Rate, len(in))
	for k, n := range in {
		cur := quote(k)
		r, ok := new(big.Rat).SetString(n.String())
		if !money.ValidCurrency(cur) || !ok || r.Sign() <= 0 {
			continue
		}
		rate, err := money.RateFromRat(r)
		if err != nil {
			return nil, err
		}
		out[cur] = rate
	}
	if len(out) == 0 {
		return nil, errors.New("no rates in response")
	}
	return out, nil
}
