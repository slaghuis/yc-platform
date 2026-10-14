package errors_test

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

func TestConstructorsMapToStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    *ycerrors.Error
		code   ycerrors.Code
		status int
	}{
		{"not found", ycerrors.NotFound("user", "u1"), ycerrors.CodeNotFound, http.StatusNotFound},
		{"conflict", ycerrors.Conflict("Email already registered"), ycerrors.CodeConflict, http.StatusConflict},
		{"unauthorized", ycerrors.Unauthorized("Token expired"), ycerrors.CodeUnauthorized, http.StatusUnauthorized},
		{"forbidden", ycerrors.Forbidden("Role required"), ycerrors.CodeForbidden, http.StatusForbidden},
		{"upstream", ycerrors.UpstreamFailure("identity 500"), ycerrors.CodeUpstreamFailure, http.StatusBadGateway},
		{"timeout", ycerrors.UpstreamTimeout("identity"), ycerrors.CodeUpstreamTimeout, http.StatusGatewayTimeout},
		{"internal", ycerrors.Internal("find user"), ycerrors.CodeInternal, http.StatusInternalServerError},
		{"semantic", ycerrors.New(ycerrors.CodeInsufficientStock, "Out of stock"), ycerrors.CodeInsufficientStock, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.code, tt.err.Code)
			assert.Equal(t, tt.status, tt.err.Status)
		})
	}
}

func TestNotFoundKeepsIDOutOfMessage(t *testing.T) {
	t.Parallel()
	e := ycerrors.NotFound("user", "secret-id")
	assert.Equal(t, "User not found", e.Message)
	assert.Contains(t, e.Internal, "secret-id")
}

func TestWrapPreservesChain(t *testing.T) {
	t.Parallel()
	sentinel := stderrors.New("no rows")
	err := fmt.Errorf("service: %w", ycerrors.Internal("find user").Wrap(sentinel))

	assert.ErrorIs(t, err, sentinel)
	e, ok := ycerrors.As(err)
	require.True(t, ok)
	assert.Equal(t, ycerrors.CodeInternal, e.Code)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeInternal))
	assert.False(t, ycerrors.HasCode(err, ycerrors.CodeNotFound))
}

func TestWrapDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	base := ycerrors.Internal("op")
	_ = base.Wrap(stderrors.New("x"))
	assert.NoError(t, base.Cause)
}

func TestValidationBuilder(t *testing.T) {
	t.Parallel()
	v := ycerrors.Validation()
	assert.False(t, v.HasErrors())

	v.Field("email", "required", "Email is required").
		Field("age", "out_of_range", "Must be at least 18")
	require.True(t, v.HasErrors())

	e := v.Build()
	assert.Equal(t, ycerrors.CodeValidationFailed, e.Code)
	assert.Equal(t, http.StatusBadRequest, e.Status)
	assert.Len(t, e.Fields, 2)
	assert.Equal(t, "email", e.Fields[0].Field)
}

func TestUnknownCodeDefaultsTo500(t *testing.T) {
	t.Parallel()
	c := ycerrors.Code("made_up")
	assert.False(t, c.Known())
	assert.Equal(t, http.StatusInternalServerError, c.Status())
}
