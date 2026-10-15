//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-image/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   "00000000-0000-0000-0000-0000000000a1",
		authctx.HeaderActorType: actorType,
	}}
}

// send returns the status and response headers.
func (c client) send(method, path, contentType string, body io.Reader, out any) (int, http.Header) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, body)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	noRedirect := *c.srv.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode, resp.Header
}

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&buf).Encode(body))
	}
	status, _ := c.send(method, path, "application/json", &buf, out)
	return status
}

func (c client) upload(data []byte, ownerRef string, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if ownerRef != "" {
		require.NoError(c.t, w.WriteField("owner_ref", ownerRef))
	}
	part, err := w.CreateFormFile("file", "photo")
	require.NoError(c.t, err)
	_, err = part.Write(data)
	require.NoError(c.t, err)
	require.NoError(c.t, w.Close())
	status, _ := c.send(http.MethodPost, "/admin/images", w.FormDataContentType(), &buf, out)
	return status
}

// waitFor polls until the image leaves pending/processing.
func (c client) waitFor(id string) api.Image {
	c.t.Helper()
	var img api.Image
	require.Eventually(c.t, func() bool {
		img = api.Image{}
		return c.do(http.MethodGet, "/images/"+id, nil, &img) == http.StatusOK &&
			img.Status != api.Processing && img.Status != api.Pending
	}, 30*time.Second, 100*time.Millisecond)
	return img
}

func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Content-Type")
}

func TestImages(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-image")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	s3 := testkit.S3(t, "yc-images")
	t.Setenv("YC_IMAGE_DB_URL", dbURL)
	t.Setenv("NATS_URL", testkit.NATS(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("S3_ENDPOINT", s3.Endpoint)
	t.Setenv("S3_ACCESS_KEY_ID", s3.AccessKey)
	t.Setenv("S3_SECRET_ACCESS_KEY", s3.SecretKey)
	t.Setenv("S3_PUBLIC_BASE_URL", s3.Endpoint+"/"+s3.Bucket)
	t.Setenv("YC_IMAGE__LIMITS__MAX_UPLOAD_BYTES", "4000000")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-image", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	eventsOf := func(tenant, eventType string) int {
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2`, tenant, eventType).Scan(&n))
		return n
	}

	admin := as(t, srv, "shop-a", "shop_admin")
	visitor := as(t, srv, "shop-a", "visitor")
	other := as(t, srv, "shop-b", "shop_admin")
	photo := pngImage(t, 1200, 800)
	var ready api.Image

	t.Run("direct upload is processed into variants", func(t *testing.T) {
		var img api.Image
		require.Equal(t, http.StatusAccepted, admin.upload(photo, "product:0190a1b2-0000-7000-8000-000000000001", &img))
		assert.Equal(t, api.Processing, img.Status)
		assert.Equal(t, "image/png", img.ContentType)

		ready = admin.waitFor(img.ImageID.String())
		require.Equal(t, api.Ready, ready.Status, "failure: %v", ready.FailureReason)
		assert.Equal(t, 1200, *ready.Width)
		assert.Equal(t, 800, *ready.Height)
		require.Len(t, ready.Variants, 9)

		sizes := map[string][2]int{}
		for _, v := range ready.Variants {
			sizes[string(v.Variant)+"."+string(v.Format)] = [2]int{v.Width, v.Height}
		}
		assert.Equal(t, [2]int{1200, 800}, sizes["original.png"])
		assert.Equal(t, [2]int{1200, 800}, sizes["xlarge.webp"], "never enlarged")
		assert.Equal(t, [2]int{1024, 683}, sizes["large.jpeg"])
		assert.Equal(t, [2]int{512, 341}, sizes["medium.webp"])
		assert.Equal(t, [2]int{200, 133}, sizes["thumbnail.jpeg"])

		for _, v := range ready.Variants {
			status, ct := fetch(t, v.URL)
			assert.Equal(t, http.StatusOK, status, v.URL)
			want := map[api.VariantFormat]string{"png": "image/png", "webp": "image/webp", "jpeg": "image/jpeg"}[v.Format]
			assert.Equal(t, want, ct, v.URL)
		}
		assert.Equal(t, 1, eventsOf("shop-a", events.TypeImageUploadedV1))
		assert.Equal(t, 1, eventsOf("shop-a", events.TypeImageProcessedV1))
	})

	t.Run("visitors see ready images and variant redirects", func(t *testing.T) {
		id := ready.ImageID.String()
		require.Equal(t, http.StatusOK, visitor.do(http.MethodGet, "/images/"+id, nil, nil))

		redirect := func(path string) string {
			status, hdr := visitor.send(http.MethodGet, "/images/"+id+"/variants/"+path, "", nil, nil)
			assert.Equal(t, http.StatusFound, status, path)
			return hdr.Get("Location")
		}
		assert.True(t, strings.HasSuffix(redirect("medium"), "/medium.webp"))
		assert.True(t, strings.HasSuffix(redirect("thumbnail?format=jpeg"), "/thumbnail.jpg"))
		assert.True(t, strings.HasSuffix(redirect("original"), "/original.png"))

		assert.Equal(t, http.StatusNotFound, other.do(http.MethodGet, "/images/"+id, nil, nil), "other shops cannot see it")
		assert.Equal(t, http.StatusForbidden, visitor.upload(photo, "", nil))
	})

	t.Run("invalid uploads are rejected", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, admin.upload([]byte("not an image"), "", nil))
		assert.Equal(t, http.StatusBadRequest, admin.upload(photo, "nonsense", nil), "owner_ref")
		big := make([]byte, 4_000_001)
		copy(big, photo)
		assert.Equal(t, http.StatusBadRequest, admin.upload(big, "", nil), "too large")
	})

	t.Run("an upload that cannot be rendered fails", func(t *testing.T) {
		var img api.Image
		require.Equal(t, http.StatusAccepted, admin.upload(photo[:len(photo)/2], "", &img), "the header is valid")
		img = admin.waitFor(img.ImageID.String())
		assert.Equal(t, api.Failed, img.Status)
		assert.Equal(t, "corrupt", *img.FailureReason)
		assert.Empty(t, img.Variants)
		assert.Equal(t, 1, eventsOf("shop-a", events.TypeImageProcessingFailedV1))
	})

	presign := func(t *testing.T, contentType string, data []byte) api.UploadURL {
		t.Helper()
		var up api.UploadURL
		require.Equal(t, http.StatusCreated, admin.do(http.MethodPost, "/admin/images/upload-url",
			map[string]any{"content_type": contentType, "bytes": len(data)}, &up))
		assert.Equal(t, api.Pending, up.Image.Status)
		return up
	}
	put := func(t *testing.T, up api.UploadURL, contentType string, data []byte) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("presigned upload then finalise", func(t *testing.T) {
		up := presign(t, "image/png", photo)
		id := up.Image.ImageID.String()
		assert.Equal(t, "image/png", up.Headers["Content-Type"])
		assert.Equal(t, http.StatusConflict, admin.do(http.MethodPost, "/admin/images/"+id+"/finalize", nil, nil), "nothing uploaded yet")
		assert.Equal(t, http.StatusNotFound, visitor.do(http.MethodGet, "/images/"+id, nil, nil), "pending is admin-only")

		assert.Equal(t, http.StatusForbidden, put(t, up, "text/html", photo), "content type is signed")
		require.Equal(t, http.StatusOK, put(t, up, "image/png", photo))
		var img api.Image
		require.Equal(t, http.StatusAccepted, admin.do(http.MethodPost, "/admin/images/"+id+"/finalize", nil, &img))
		assert.Equal(t, api.Processing, img.Status)
		assert.Equal(t, api.Ready, admin.waitFor(id).Status)
		assert.Equal(t, http.StatusConflict, admin.do(http.MethodPost, "/admin/images/"+id+"/finalize", nil, nil))
	})

	t.Run("invalid presigned upload fails the image", func(t *testing.T) {
		junk := []byte("definitely not a png")
		up := presign(t, "image/png", junk)
		id := up.Image.ImageID.String()
		require.Equal(t, http.StatusOK, put(t, up, "image/png", junk))
		assert.Equal(t, http.StatusBadRequest, admin.do(http.MethodPost, "/admin/images/"+id+"/finalize", nil, nil))

		var img api.Image
		require.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/images/"+id, nil, &img))
		assert.Equal(t, api.Failed, img.Status)
		assert.Equal(t, "unsupported_format", *img.FailureReason)
		assert.Equal(t, 2, eventsOf("shop-a", events.TypeImageProcessingFailedV1))
	})

	t.Run("delete removes the image and its files", func(t *testing.T) {
		id := ready.ImageID.String()
		assert.Equal(t, http.StatusNotFound, other.do(http.MethodDelete, "/admin/images/"+id, nil, nil))
		require.Equal(t, http.StatusNoContent, admin.do(http.MethodDelete, "/admin/images/"+id, nil, nil))
		assert.Equal(t, http.StatusNotFound, admin.do(http.MethodGet, "/images/"+id, nil, nil))
		assert.Equal(t, http.StatusNotFound, admin.do(http.MethodDelete, "/admin/images/"+id, nil, nil))
		for _, v := range ready.Variants {
			status, _ := fetch(t, v.URL)
			assert.Equal(t, http.StatusNotFound, status, v.URL)
		}
		assert.Equal(t, 1, eventsOf("shop-a", events.TypeImageDeletedV1))
	})
}
