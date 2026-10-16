//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
	admin.login(t, email, password)
	s.sys = admin
	s.awaitRoute(t, admin, "/sysadmin/gdpr/register")
	s.awaitRoute(t, admin, "/sysadmin/tenants")
	return s
}

// provision creates a shop with yc-tenant-cli and waits for the gateway to
// map its hostname.
func (s *stack) provision(t *testing.T, tenant string) *client {
	t.Helper()
	host := tenant + ".localhost"
	adminHost := envOr("YC_E2E_ADMIN_HOST", "admin.localhost")
	gwURL := envOr("YC_GATEWAY_URL", "http://"+adminHost+":8090")
	s.run(t, "go", "run", "./tools/yc-tenant-cli", "provision",
		"--token", s.sys.token,
		"--gateway-url", gwURL,
		"--tenant-id", tenant, "--name", "E2E "+tenant,
		"--hostname", host,
		"--contact-email", "ops@"+tenant+".test",
		"--store-currency", "ZAR", "--default-jurisdiction", "ZA")
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

// shopAdmin registers a customer on shop, has the system admin promote
// them, and returns them signed in as the shop admin.
func (s *stack) shopAdmin(t *testing.T, shop *client) *client {
	t.Helper()
	email, password := fmt.Sprintf("owner-%d@e2e.test", time.Now().UnixNano()), "e2e-owner-password"
	_, userID := shop.register(t, email, password)
	s.sys.must(t, http.MethodPost, "/admin/users/"+userID+"/promote", map[string]string{"role": "shop_admin"},
		http.StatusOK, nil)
	admin := s.client(shop.host)
	admin.login(t, email, password)
	return admin
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

// as returns a client for the same caller on another host.
func (c *client) as(host string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{gateway: c.gateway, host: host, token: c.token, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
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

func (c *client) login(t *testing.T, email, password string) {
	t.Helper()
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	c.must(t, http.MethodPost, "/login", map[string]string{"email": email, "password": password}, http.StatusOK, &tokens)
	c.token = tokens.AccessToken
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp := c.send(t, req)
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

// send sends req as the client: its host, token and cookies.
func (c *client) send(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	req.Host = c.host
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
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
