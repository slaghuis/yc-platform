package httpkit_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func newTestRouter(checks ...httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:      httpkit.BuildInfo{Service: "yc-test", Version: "1.2.3"},
		Checks:     checks,
		Middleware: []func(http.Handler) http.Handler{authctx.Middleware},
	})
	r.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, _ := authctx.FromContext(r.Context())
		httpkit.WriteJSON(w, http.StatusOK, map[string]string{
			"tenant":      a.TenantID,
			"correlation": a.CorrelationID,
		})
	})
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	return r
}

func get(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthEndpoints(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	failing := httpkit.Check{Name: "database", Fn: func(context.Context) error { return stderrors.New("down") }}
	ok := httpkit.Check{Name: "nats", Fn: func(context.Context) error { return nil }}

	h := newTestRouter(ok)
	assert.Equal(t, http.StatusOK, get(t, h, "/healthz", nil).Code)

	rec := get(t, h, "/readyz", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = get(t, newTestRouter(ok, failing), "/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var rd httpkit.Readiness
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rd))
	assert.Equal(t, "not_ready", rd.Status)
	assert.Equal(t, map[string]string{"database": "failing", "nats": "ok"}, rd.Checks)
	assert.NotContains(t, rec.Body.String(), "down", "check errors stay in logs")

	rec = get(t, h, "/version", nil)
	assert.JSONEq(t, `{"service":"yc-test","version":"1.2.3"}`, rec.Body.String())
}

func TestCorrelationIDGeneratedAndPropagated(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	h := newTestRouter()

	rec := get(t, h, "/items/1", nil)
	generated := rec.Header().Get(authctx.HeaderCorrelationID)
	_, err := uuid.Parse(generated)
	require.NoError(t, err)

	given := uuid.NewString()
	rec = get(t, h, "/items/1", map[string]string{
		authctx.HeaderCorrelationID: given,
		authctx.HeaderTenantID:      "shop-abc",
	})
	assert.Equal(t, given, rec.Header().Get(authctx.HeaderCorrelationID))
	assert.JSONEq(t, `{"tenant":"shop-abc","correlation":"`+given+`"}`, rec.Body.String())

	rec = get(t, h, "/items/1", map[string]string{authctx.HeaderCorrelationID: "debug-not-a-uuid"})
	assert.NotEqual(t, "debug-not-a-uuid", rec.Header().Get(authctx.HeaderCorrelationID))
}

func TestAccessLogCarriesRouteAndTenant(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-test")
	h := newTestRouter()

	get(t, h, "/items/42", map[string]string{
		authctx.HeaderTenantID: "shop-abc", authctx.HeaderActorID: "u1", authctx.HeaderActorType: "visitor",
	})

	var entry map[string]any
	for _, line := range buf.Lines() {
		if strings.Contains(line, `"msg":"request"`) {
			require.NoError(t, json.Unmarshal([]byte(line), &entry))
		}
	}
	require.NotNil(t, entry, "access log line")
	assert.Equal(t, "/items/{id}", entry["http.route"])
	assert.InDelta(t, 200, entry["http.status"], 0)
	assert.Equal(t, "shop-abc", entry["tenant_id"])
	assert.Equal(t, "visitor", entry["actor_type"])
	assert.NotEmpty(t, entry["correlation_id"])
}

func TestRecovererAndNotFound(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-test")
	h := newTestRouter()

	rec := get(t, h, "/panic", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "kaboom")
	assert.Contains(t, buf.String(), "panic recovered")

	rec = get(t, h, "/nope", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, ycerrors.ContentType, rec.Header().Get("Content-Type"))
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	type body struct {
		Name string `json:"name"`
	}
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"ok", `{"name":"x"}`, ""},
		{"empty", ``, "Request body is required"},
		{"unknown field", `{"nme":"x"}`, "Invalid JSON"},
		{"trailing", `{"name":"x"} {}`, "Invalid JSON"},
		{"too large", `{"name":"` + strings.Repeat("a", httpkit.MaxBodyBytes) + `"}`, "Request body too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.in))
			var b body
			err := httpkit.DecodeJSON(httptest.NewRecorder(), req, &b)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "x", b.Name)
				return
			}
			e, ok := ycerrors.As(err)
			require.True(t, ok)
			assert.Equal(t, ycerrors.CodeValidationFailed, e.Code)
			assert.Equal(t, tt.wantErr, e.Message)
		})
	}
}

func TestClientPropagatesCorrelationAndAuth(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		httpkit.WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}))
	defer upstream.Close()

	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "shop-abc", ActorID: "u1", ActorType: authctx.ActorCustomer, CorrelationID: "c-1", Token: "tok",
	})

	var out map[string]string
	require.NoError(t, httpkit.DoJSON(ctx, httpkit.NewClient(httpkit.ClientOptions{}), http.MethodGet, upstream.URL, nil, &out))
	assert.Equal(t, "yes", out["ok"])
	assert.Equal(t, "c-1", got.Get(authctx.HeaderCorrelationID))
	assert.NotEmpty(t, got.Get("Traceparent"))
	assert.Empty(t, got.Get("Authorization"), "auth is opt-in")

	require.NoError(t, httpkit.DoJSON(ctx, httpkit.NewClient(httpkit.ClientOptions{PropagateAuth: true}), http.MethodGet, upstream.URL, nil, nil))
	assert.Equal(t, "Bearer tok", got.Get("Authorization"))
	assert.Equal(t, "shop-abc", got.Get(authctx.HeaderTenantID))
}

func TestDoJSONClassifiesFailures(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	c := httpkit.NewClient(httpkit.ClientOptions{Timeout: 50 * time.Millisecond})

	err := httpkit.DoJSON(context.Background(), c, http.MethodPost, upstream.URL+"/x", map[string]string{"a": "b"}, nil)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeUpstreamFailure))
	assert.Equal(t, http.StatusUnauthorized, httpkit.UpstreamStatus(err))

	err = httpkit.DoJSON(context.Background(), c, http.MethodGet, upstream.URL+"/slow", nil, nil)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeUpstreamTimeout), "got %v", err)
}

func TestServeShutsDownGracefully(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := httpkit.DefaultServerConfig()
	cfg.ShutdownTimeout = time.Second
	go func() { done <- httpkit.ServeListener(ctx, cfg, ln, newTestRouter()) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestServeDrainsBeforeShutdown(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	base := "http://" + ln.Addr().String()
	status := func(path string) int {
		resp, err := http.Get(base + path)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := httpkit.DefaultServerConfig()
	cfg.ShutdownDelay = 500 * time.Millisecond
	go func() { done <- httpkit.ServeListener(ctx, cfg, ln, newTestRouter()) }()
	require.Eventually(t, func() bool { return status("/readyz") == http.StatusOK }, 2*time.Second, 20*time.Millisecond)
	assert.Equal(t, 0, httpkit.Probe([]string{base + "/healthz"}))

	cancel()
	require.Eventually(t, func() bool { return status("/readyz") == http.StatusServiceUnavailable },
		400*time.Millisecond, 10*time.Millisecond, "unready while draining")
	assert.Equal(t, http.StatusOK, status("/healthz"), "still serving while draining")
	require.NoError(t, <-done)
	assert.Equal(t, 1, httpkit.Probe([]string{base + "/healthz"}), "closed after the delay")
}
