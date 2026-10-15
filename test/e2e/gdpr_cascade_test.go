//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

// TestGDPRCascade deletes accounts through yc-gateway and follows the
// cascade (ADR-005) to completion in yc-audit's register: every expected
// service reports, the account can no longer sign in, and the deletion is
// audited. Needs the full stack (task devstack:up && task devstack:app).
func TestGDPRCascade(t *testing.T) {
	s := newStack(t)
	tenant := fmt.Sprintf("e2e-gdpr-%d", time.Now().Unix())
	shop := s.provision(t, tenant)

	t.Run("a customer deletes their own account", func(t *testing.T) {
		email, password := fmt.Sprintf("self-%d@e2e.test", time.Now().UnixNano()), "e2e-password-1"
		customer, userID := shop.register(t, email, password)
		customer.must(t, http.MethodDelete, "/me", nil, http.StatusNoContent, nil)

		entry := s.awaitComplete(t, userID)
		if entry.TenantID != tenant || entry.Role != "customer" || entry.RequestedBy != "user" {
			t.Errorf("entry = %+v, want a customer's own request in %s", entry, tenant)
		}
		s.assertReported(t, entry)

		status, _ := shop.do(t, http.MethodPost, "/login", map[string]string{"email": email, "password": password}, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("login after deletion = %d, want 401", status)
		}
		s.assertAudited(t, tenant, userID)
	})

	t.Run("a system admin deletes a customer", func(t *testing.T) {
		_, userID := shop.register(t, fmt.Sprintf("admin-deleted-%d@e2e.test", time.Now().UnixNano()), "e2e-password-2")
		s.sys.must(t, http.MethodDelete, "/admin/users/"+userID, nil, http.StatusNoContent, nil)

		entry := s.awaitComplete(t, userID)
		if entry.RequestedBy != "system_admin" {
			t.Errorf("requested_by = %q, want system_admin", entry.RequestedBy)
		}
		s.assertReported(t, entry)

		status, _ := s.sys.do(t, http.MethodDelete, "/admin/users/"+userID, nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("deleting again = %d, want 404", status)
		}
	})

	t.Run("system admins cannot be deleted", func(t *testing.T) {
		var me struct {
			UserID string `json:"user_id"`
		}
		s.sys.must(t, http.MethodGet, "/me", nil, http.StatusOK, &me)
		status, _ := s.sys.do(t, http.MethodDelete, "/admin/users/"+me.UserID, nil, nil)
		if status != http.StatusConflict {
			t.Errorf("deleting a system admin = %d, want 409", status)
		}
	})
}

type registerEntry struct {
	TenantID         string   `json:"tenant_id"`
	UserID           string   `json:"user_id"`
	Pseudonym        string   `json:"pseudonym"`
	Role             string   `json:"role"`
	RequestedBy      string   `json:"requested_by"`
	Status           string   `json:"status"`
	ExpectedServices []string `json:"expected_services"`
	MissingServices  []string `json:"missing_services"`
	Reports          []struct {
		Service     string `json:"service"`
		Action      string `json:"action"`
		RecordCount int    `json:"record_count"`
	} `json:"reports"`
}

// awaitComplete polls the GDPR register until the user's deletion is
// complete.
func (s *stack) awaitComplete(t *testing.T, userID string) registerEntry {
	t.Helper()
	var entry registerEntry
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, _ := s.sys.do(t, http.MethodGet, "/sysadmin/gdpr/register/"+userID, nil, &entry)
		if status == http.StatusOK && entry.Status == "complete" {
			return entry
		}
		if time.Now().After(deadline) {
			t.Fatalf("GDPR deletion of %s not complete: HTTP %d, %+v", userID, status, entry)
		}
		time.Sleep(time.Second)
	}
}

// assertReported checks every expected service reported, with a pseudonym
// that is not the user's ID.
func (s *stack) assertReported(t *testing.T, entry registerEntry) {
	t.Helper()
	if entry.Pseudonym == "" || entry.Pseudonym == entry.UserID {
		t.Errorf("pseudonym = %q", entry.Pseudonym)
	}
	if len(entry.MissingServices) != 0 {
		t.Errorf("missing services: %v", entry.MissingServices)
	}
	for _, want := range []string{"yc-identity", "yc-preference", "yc-cart", "yc-orders", "yc-analytics"} {
		if !slices.Contains(entry.ExpectedServices, want) {
			t.Errorf("%s is not an expected service: %v", want, entry.ExpectedServices)
		}
	}
	reported := map[string]bool{}
	for _, r := range entry.Reports {
		reported[r.Service] = true
	}
	for _, svc := range entry.ExpectedServices {
		if !reported[svc] {
			t.Errorf("no report from %s", svc)
		}
	}
}

// assertAudited waits for yc-audit to record the deletion, each service's
// report and the completion under the user.
func (s *stack) assertAudited(t *testing.T, tenant, userID string) {
	t.Helper()
	q := url.Values{"category": {"gdpr"}, "tenant_id": {tenant}, "subject_ref": {"user:" + userID}, "page_size": {"100"}}
	want := []string{"identity.user.deleted.v1", "platform.gdpr.cascade_completed.v1", "audit.gdpr.cascade_complete.v1"}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var list struct {
			Items []struct {
				EventType string `json:"event_type"`
			} `json:"items"`
		}
		s.sys.must(t, http.MethodGet, "/sysadmin/audit?"+q.Encode(), nil, http.StatusOK, &list)
		types := map[string]bool{}
		for _, r := range list.Items {
			types[r.EventType] = true
		}
		missing := slices.DeleteFunc(slices.Clone(want), func(w string) bool { return types[w] })
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %v audit records for user:%s (have %v)", missing, userID, types)
		}
		time.Sleep(time.Second)
	}
}
