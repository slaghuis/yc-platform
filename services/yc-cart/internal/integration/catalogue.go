package integration

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls (httpkit.InternalClient).
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
	Do(ctx context.Context, method, path string, in, out any) error
}

// Catalogue looks up SKUs and product images for recovery emails.
type Catalogue struct {
	client Caller
}

// NewCatalogue returns a Catalogue client.
func NewCatalogue(client Caller) *Catalogue {
	return &Catalogue{client: client}
}

// SKU is a sellable unit with its product name.
type SKU struct {
	SKUID       string `json:"sku_id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
}

type skuBatch struct {
	SKUs []SKU `json:"skus"`
}

// SKUs looks up SKUs by id (yc-product /internal/skus/batch).
func (c *Catalogue) SKUs(ctx context.Context, ids []string) (map[string]SKU, error) {
	out := make(map[string]SKU, len(ids))
	if len(ids) == 0 || c == nil || c.client == nil {
		return out, nil
	}
	var res skuBatch
	q := url.Values{"ids": {strings.Join(ids, ",")}}
	err := c.client.Call(ctx, http.MethodGet, "/internal/skus/batch?"+q.Encode(),
		httpkit.CallOptions{Attempts: 2}, nil, &res)
	if err != nil {
		return out, err
	}
	for _, s := range res.SKUs {
		out[s.SKUID] = s
	}
	return out, nil
}

type productImage struct {
	ImageID   string `json:"image_id"`
	IsPrimary bool   `json:"is_primary"`
}

type product struct {
	Images []productImage `json:"images"`
}

type imageVariant struct {
	Variant string `json:"variant"`
	URL     string `json:"url"`
}

type image struct {
	Variants []imageVariant `json:"variants"`
}

// PrimaryImageURL returns a product's primary (or first) image URL, preferably
// the medium or thumbnail variant. Empty when unknown.
func (c *Catalogue) PrimaryImageURL(ctx context.Context, productID string) string {
	if productID == "" || c == nil || c.client == nil {
		return ""
	}
	var p product
	if err := c.client.Call(ctx, http.MethodGet, "/products/"+url.PathEscape(productID),
		httpkit.CallOptions{Attempts: 1}, nil, &p); err != nil || len(p.Images) == 0 {
		return ""
	}
	imageID := p.Images[0].ImageID
	for _, img := range p.Images {
		if img.IsPrimary {
			imageID = img.ImageID
			break
		}
	}
	var im image
	if err := c.client.Call(ctx, http.MethodGet, "/images/"+url.PathEscape(imageID),
		httpkit.CallOptions{Attempts: 1}, nil, &im); err != nil {
		return ""
	}
	prefer := map[string]int{"medium": 3, "thumbnail": 2, "large": 1, "xlarge": 0, "original": -1}
	best, bestScore := "", -2
	for _, v := range im.Variants {
		if score, ok := prefer[v.Variant]; ok && score > bestScore && v.URL != "" {
			best, bestScore = v.URL, score
		}
	}
	return best
}
