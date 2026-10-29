package integration

import (
	"context"
	"net/http"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Support is yc-support's claim API (issue #33).
type Support struct {
	client Caller
}

// NewSupport returns the client.
func NewSupport(client Caller) *Support {
	return &Support{client: client}
}

// ClaimSupportRequests attaches unclaimed threads for email to userID.
func (s *Support) ClaimSupportRequests(ctx context.Context, userID, email string) error {
	return s.client.Call(ctx, http.MethodPost, "/internal/support/requests/claim",
		httpkit.CallOptions{Attempts: 2},
		map[string]string{"user_id": userID, "email": email}, nil)
}
