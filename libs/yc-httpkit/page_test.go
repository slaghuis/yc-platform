package httpkit_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func TestParsePage(t *testing.T) {
	p, err := httpkit.ParsePage(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, httpkit.PageRequest{Size: httpkit.DefaultPageSize}, p)

	size, cursor := 50, "abc"
	p, err = httpkit.ParsePage(&size, &cursor)
	require.NoError(t, err)
	assert.Equal(t, httpkit.PageRequest{Size: 50, Cursor: "abc"}, p)

	for _, bad := range []int{0, -1, httpkit.MaxPageSize + 1} {
		_, err := httpkit.ParsePage(&bad, nil)
		assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed), "page_size %d", bad)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	type key struct {
		Name string `json:"n"`
		ID   string `json:"i"`
	}
	c := httpkit.EncodeCursor(key{Name: "Acme", ID: "42"})
	assert.NotContains(t, c, "Acme", "cursors are opaque")

	var got key
	require.NoError(t, httpkit.DecodeCursor(c, &got))
	assert.Equal(t, key{Name: "Acme", ID: "42"}, got)

	err := httpkit.DecodeCursor("not a cursor!", &got)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))
}

func TestGeneratedServerErrorHandlers(t *testing.T) {
	telemetry.InitForTest(t, "httpkit-test")
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, error)
		err     error
		status  int
		detail  string
	}{
		{"param", httpkit.ParamError, errors.New("invalid format for parameter page_size"), http.StatusBadRequest, "page_size"},
		{"body", httpkit.RequestError, errors.New("can't decode JSON body"), http.StatusBadRequest, "Invalid request body"},
		{"body too large", httpkit.RequestError, &http.MaxBytesError{Limit: 1}, http.StatusBadRequest, "too large"},
		{"yc error", httpkit.ResponseError, ycerrors.NotFound("Product", "p1"), http.StatusNotFound, ""},
		{"plain error", httpkit.ResponseError, errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.handler(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody), tt.err)
			assert.Equal(t, tt.status, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.detail)
			if tt.status == http.StatusInternalServerError {
				assert.NotContains(t, rec.Body.String(), "boom")
			}
		})
	}
}

func TestLimitBody(t *testing.T) {
	h := httpkit.LimitBody(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 16)
		_, err := r.Body.Read(buf)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("0123456789")))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
