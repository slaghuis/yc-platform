//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
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

// stack is the running compose stack, reached through yc-gateway.
type stack struct {
	gateway string
	root    string
	sys     *client
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{gateway: envOr("YC_E2E_GATEWAY_URL", "http://localhost:8090"), root: "../.."}
	resp, err := http.Get(s.gateway + "/readyz")
	if err != nil {
		t.Fatalf("yc-gateway at %s: %v (task devstack:up && task devstack:app)", s.gateway, err)
	}
	_ = resp.Body.Close()
	email := envOr("YC_E2E_SYSADMIN_EMAIL", "sysadmin@yc.test")
	password := envOr("YC_E2E_SYSADMIN_PASSWORD", "correct-horse-battery")
	s.run(t, "docker", "exec", "-e", "YC_BOOTSTRAP_ADMIN_PASSWORD="+password, "yc-identity",
		"/app/service", "bootstrap-admin", "--email", email)
	admin := s.client(envOr("YC_E2E_ADMIN_HOST", "admin.localhost"))
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	admin.must(t, http.MethodPost, "/login", map[string]string{"email": email, "password": password}, http.StatusOK, &tokens)
	admin.token = tokens.AccessToken
	s.sys = admin
	s.awaitRoute(t, admin, "/sysadmin/gdpr/register")
	return s
}

// provision creates a shop with yc-tenant-cli and waits for the gateway to
// map its hostname.
func (s *stack) provision(t *testing.T, tenant string) *client {
	t.Helper()
	host := tenant + ".localhost"
	s.run(t, "go", "run", "./tools/yc-tenant-cli", "provision", "--tenant-id", tenant, "--name", "E2E "+tenant,
		"--hostname", host, "--store-currency", "ZAR", "--default-jurisdiction", "ZA")
	shop := s.client(host)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if status, _ := shop.do(t, http.MethodGet, "/me", nil, nil); status == http.StatusOK {
			return shop
		}
		if time.Now().After(deadline) {
			t.Fatalf("yc-gateway did not map %s", host)
		}
		time.Sleep(time.Second)
	}
}

// awaitRoute waits for a service to register the path with the gateway.
func (s *stack) awaitRoute(t *testing.T, c *client, path string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, body := c.do(t, http.MethodGet, path, nil, nil)
		if !strings.Contains(string(body), "Route not found") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no gateway route for %s", path)
		}
		time.Sleep(time.Second)
	}
}

func (s *stack) run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// client calls the gateway as one host, with a cookie jar for the
// visitor session and an optional bearer token.
type client struct {
	gateway string
	host    string
	token   string
	http    *http.Client
}

func (s *stack) client(host string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{gateway: s.gateway, host: host, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

// register signs a visitor up as a customer and returns them signed in.
func (c *client) register(t *testing.T, email, password string) (*client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	customer := &client{gateway: c.gateway, host: c.host, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	customer.must(t, http.MethodGet, "/me", nil, http.StatusOK, nil)
	var out struct {
		AccessToken string `json:"access_token"`
		UserID      string `json:"user_id"`
	}
	customer.must(t, http.MethodPost, "/register", map[string]string{"email": email, "password": password},
		http.StatusCreated, &out)
	customer.token = out.AccessToken
	return customer, out.UserID
}

func (c *client) do(t *testing.T, method, path string, body, out any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.gateway+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = c.host
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && resp.StatusCode < 300 && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode: %v\n%s", method, path, err, data)
		}
	}
	return resp.StatusCode, data
}

func (c *client) must(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	if status, data := c.do(t, method, path, body, out); status != want {
		t.Fatalf("%s %s%s = %d, want %d\n%s", method, c.host, path, status, want, data)
	}
}

func envOr(key, otherwise string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return otherwise
}
