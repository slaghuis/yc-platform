package errors_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func writeError(t *testing.T, err error) (*httptest.ResponseRecorder, ycerrors.Problem) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", http.NoBody)
	req = req.WithContext(telemetry.ContextWithAuthAttrs(req.Context(),
		telemetry.AuthAttrs{CorrelationID: "corr-1"}))
	rec := httptest.NewRecorder()

	ycerrors.WriteError(rec, req, err)

	var p ycerrors.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	return rec, p
}

func TestWriteErrorProblemJSON(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-errors-test")

	err := ycerrors.Validation().Field("email", "required", "Email is required").Build()
	rec, p := writeError(t, err)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, ycerrors.ContentType, rec.Header().Get("Content-Type"))
	assert.Equal(t, "yc:validation_failed", p.Type)
	assert.Equal(t, "Validation failed", p.Title)
	assert.Equal(t, ycerrors.CodeValidationFailed, p.Code)
	assert.Equal(t, "corr-1", p.CorrelationID)
	require.Len(t, p.Errors, 1)
	assert.Equal(t, "email", p.Errors[0].Field)
	assert.Contains(t, buf.String(), `"level":"INFO"`)
}

func TestWriteErrorExtensions(t *testing.T) {
	telemetry.InitForTest(t, "yc-errors-test")

	base := ycerrors.New(ycerrors.CodeInsufficientStock, "Not enough stock")
	err := base.With("shortages", []map[string]any{{"sku_id": "a", "requested": 2, "available": 1}})
	assert.Nil(t, base.Extensions, "With copies")
	rec, p := writeError(t, err)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), `"shortages":[{"available":1,"requested":2,"sku_id":"a"}]`)
	assert.Equal(t, ycerrors.CodeInsufficientStock, p.Code)
	assert.Equal(t, []any{map[string]any{"sku_id": "a", "requested": float64(2), "available": float64(1)}}, p.Extensions["shortages"])
	assert.NotContains(t, p.Extensions, "code")
	assert.Panics(t, func() { base.With("status", 1) })
}

func TestWriteErrorHidesInternals(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-errors-test")

	err := ycerrors.Internal("select users").Wrap(stderrors.New("pq: relation users does not exist"))
	rec, p := writeError(t, err)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "Internal error", p.Detail)
	assert.NotContains(t, rec.Body.String(), "relation users")
	assert.NotContains(t, rec.Body.String(), "select users")

	logs := buf.String()
	assert.Contains(t, logs, `"level":"ERROR"`)
	assert.Contains(t, logs, "relation users")
	assert.Contains(t, logs, "select users")
}

func TestWriteErrorUnclassified(t *testing.T) {
	telemetry.InitForTest(t, "yc-errors-test")

	rec, p := writeError(t, stderrors.New("boom"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, ycerrors.CodeInternal, p.Code)
	assert.NotContains(t, rec.Body.String(), "boom")
}

func TestWriteErrorDeadline(t *testing.T) {
	telemetry.InitForTest(t, "yc-errors-test")

	rec, p := writeError(t, context.DeadlineExceeded)
	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Equal(t, ycerrors.CodeUpstreamTimeout, p.Code)
}
