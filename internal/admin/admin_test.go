package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paibot-hq/paigate/internal/store"
)

const adminToken = "test-admin-token-0123456789abcdefghij"

type memStore struct {
	mu   sync.Mutex
	keys []store.Key
	next int64
}

func (m *memStore) Keys(context.Context) ([]store.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Key(nil), m.keys...), nil
}

func (m *memStore) Key(_ context.Context, name string) (store.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.Name == name {
			return k, nil
		}
	}
	return store.Key{}, store.ErrNotFound
}

func (m *memStore) CreateKey(_ context.Context, name, hash string, set store.KeySettings) (store.Key, error) {
	if err := set.Validate(); err != nil {
		return store.Key{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.Name == name {
			return store.Key{}, store.ErrExists
		}
	}
	m.next++
	k := store.Key{ID: m.next, Name: name, Hash: hash, Owner: set.Owner, Models: set.Models, RPM: set.RPM, MonthlyTokenBudget: set.MonthlyTokenBudget, ExpiresAt: set.ExpiresAt, CreatedAt: time.Now()}
	m.keys = append(m.keys, k)
	return k, nil
}

func (m *memStore) UpdateKey(_ context.Context, name string, set store.KeySettings) error {
	if err := set.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, k := range m.keys {
		if k.Name == name && k.RevokedAt == nil {
			m.keys[i].Owner, m.keys[i].Models, m.keys[i].RPM = set.Owner, set.Models, set.RPM
			m.keys[i].MonthlyTokenBudget, m.keys[i].ExpiresAt = set.MonthlyTokenBudget, set.ExpiresAt
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memStore) RevokeKey(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, k := range m.keys {
		if k.Name == name && k.RevokedAt == nil {
			now := time.Now()
			m.keys[i].RevokedAt = &now
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memStore) TokensSince(context.Context, time.Time) (map[int64]int64, error) {
	return map[int64]int64{1: 800}, nil
}

func (m *memStore) Report(context.Context, time.Time, time.Time) ([]store.ReportRow, error) {
	return []store.ReportRow{{Key: "tanaka", Model: "chat", Requests: 12, PromptTokens: 1000, CompletionTokens: 234}}, nil
}

type client struct {
	t    *testing.T
	srv  *httptest.Server
	http *http.Client
	csrf string
}

func newAdmin(t *testing.T) (*client, *memStore) {
	t.Helper()
	ms := &memStore{}
	s, err := New(Options{
		Store:    ms,
		Token:    adminToken,
		Models:   func() []string { return []string{"chat", "claude"} },
		Location: time.UTC,
		Log:      slog.New(slog.DiscardHandler),
		NewKey: func() (string, string) {
			k := "pg_minted-" + randomHex(8)
			sum := sha256.Sum256([]byte(k))
			return k, "sha256:" + hex.EncodeToString(sum[:])
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	c := &client{t: t, srv: srv, http: srv.Client()}
	c.http.Jar = newJar()
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c, ms
}

// a minimal cookie jar, so the test does not depend on net/http/cookiejar's
// public-suffix rules for 127.0.0.1.
type jar struct {
	mu sync.Mutex
	c  map[string]*http.Cookie
}

func newJar() *jar { return &jar{c: map[string]*http.Cookie{}} }
func (j *jar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cs {
		if c.MaxAge < 0 {
			delete(j.c, c.Name)
		} else {
			j.c[c.Name] = c
		}
	}
}
func (j *jar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, c := range j.c {
		out = append(out, c)
	}
	return out
}

func (c *client) get(path string) (int, string, http.Header) {
	c.t.Helper()
	resp, err := c.http.Get(c.srv.URL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp.StatusCode, string(b), resp.Header
}

func (c *client) post(path string, form url.Values, header ...string) (int, string, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest("POST", c.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (c *client) signIn(header ...string) {
	c.t.Helper()
	if code, body, _ := c.post("/login", url.Values{"token": {adminToken}}, header...); code != 303 {
		c.t.Fatalf("sign in: %d %s", code, body)
	}
	c.get("/") // picks up the CSRF token
}

func TestSignIn(t *testing.T) {
	c, _ := newAdmin(t)
	if code, _, h := c.get("/"); code != 303 || h.Get("Location") != "/login" {
		t.Fatalf("signed out: %d %s", code, h.Get("Location"))
	}
	if code, body, _ := c.post("/login", url.Values{"token": {"wrong"}}); code != 401 || !strings.Contains(body, "not right") {
		t.Errorf("wrong token: %d", code)
	}
	_, _, h := c.post("/login", url.Values{"token": {adminToken}})
	cookie := h.Get("Set-Cookie")
	for _, want := range []string{"HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("cookie %q lacks %s", cookie, want)
		}
	}
	code, body, h := c.get("/")
	if code != 200 || !strings.Contains(body, "No keys yet") {
		t.Fatalf("keys page: %d", code)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers: %v", h)
	}
	// Signing out ends the session.
	c.post("/logout", url.Values{"csrf": {c.csrf}})
	if code, _, _ := c.get("/"); code != 303 {
		t.Errorf("after sign out: %d", code)
	}
}

func TestSignInThrottle(t *testing.T) {
	c, _ := newAdmin(t)
	for range 5 {
		c.post("/login", url.Values{"token": {"wrong"}})
	}
	// Even the right token waits once the failures are used up.
	if code, _, _ := c.post("/login", url.Values{"token": {adminToken}}); code != 429 {
		t.Errorf("after 5 failures: %d, want 429", code)
	}
}

func TestShortTokenRefused(t *testing.T) {
	if _, err := New(Options{Token: "short"}); err == nil {
		t.Error("accepted a short admin token")
	}
}

func TestCSRF(t *testing.T) {
	c, ms := newAdmin(t)
	c.signIn()
	form := url.Values{"name": {"x"}, "models": {"chat"}}
	if code, _, _ := c.post("/keys/new", form); code != 403 {
		t.Errorf("no CSRF token: %d", code)
	}
	form.Set("csrf", c.csrf)
	if code, _, _ := c.post("/keys/new", form, "Origin", "https://evil.example"); code != 403 {
		t.Errorf("cross-origin: %d", code)
	}
	if code, _, _ := c.post("/keys/new", form, "Sec-Fetch-Site", "cross-site"); code != 403 {
		t.Errorf("cross-site: %d", code)
	}
	if code, _, _ := c.post("/keys/new", form, "Sec-Fetch-Site", "same-site", "Origin", "http://other.example.com"); code != 403 {
		t.Errorf("same-site: %d", code)
	}
	if len(ms.keys) != 0 {
		t.Error("a forged request created a key")
	}
}

// Browsers send Origin: null when the referrer policy hides it; the
// Sec-Fetch-Site header still says the form came from this page.
func TestSignInFromBrowser(t *testing.T) {
	c, _ := newAdmin(t)
	c.signIn("Sec-Fetch-Site", "same-origin", "Origin", "null")
	if c.csrf == "" {
		t.Fatal("not signed in")
	}
}

func TestKeyLifecycle(t *testing.T) {
	c, ms := newAdmin(t)
	c.signIn()

	// Bad input goes back to the form with the problem stated.
	for _, f := range []url.Values{
		{"name": {"tanaka"}},                                       // no models
		{"name": {"bad name"}, "models": {"chat"}},                 // bad name
		{"name": {"tanaka"}, "models": {"gpt-secret"}},             // unknown model
		{"name": {"tanaka"}, "models": {"chat"}, "budget": {"-1"}}, // bad budget
		{"name": {"tanaka"}, "models": {"chat"}, "expires": {"tomorrow"}},
	} {
		f.Set("csrf", c.csrf)
		if code, body, _ := c.post("/keys/new", f); code != 400 || !strings.Contains(body, `class="alert error"`) {
			t.Errorf("%v: %d", f, code)
		}
	}

	form := url.Values{"csrf": {c.csrf}, "name": {"tanaka"}, "owner": {"<script>alert(1)</script>"}, "models": {"chat", "claude"},
		"rpm": {"60"}, "budget": {"1,000"}, "expires": {"2027-03-31"}}
	code, body, _ := c.post("/keys/new", form)
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	shown := regexp.MustCompile(`<code id="newkey">(pg_[^<]+)</code>`).FindStringSubmatch(body)
	if shown == nil {
		t.Fatalf("the new key is not shown: %s", body)
	}
	sum := sha256.Sum256([]byte(shown[1]))
	k := ms.keys[0]
	if k.Hash != "sha256:"+hex.EncodeToString(sum[:]) || k.RPM != 60 || k.MonthlyTokenBudget != 1000 || len(k.Models) != 2 {
		t.Errorf("stored %+v", k)
	}
	if !k.ExpiresAt.Equal(time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expires %v", k.ExpiresAt)
	}
	if code, _, _ := c.post("/keys/new", form); code != 400 {
		t.Errorf("duplicate name: %d", code)
	}

	// The list shows it, with the owner escaped and usage against the budget.
	_, body, _ = c.get("/")
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Error("owner not escaped")
	}
	if !strings.Contains(body, "800") || !strings.Contains(body, `class="w80"`) || !strings.Contains(body, "2027-03-31") {
		t.Errorf("list lacks usage or expiry: %s", body)
	}

	// Edit.
	code, _, h := c.post("/keys/tanaka", url.Values{"csrf": {c.csrf}, "models": {"chat"}, "rpm": {""}, "budget": {""}, "expires": {""}})
	if code != 303 || !strings.Contains(h.Get("Location"), "Saved") {
		t.Fatalf("update: %d", code)
	}
	if k := ms.keys[0]; len(k.Models) != 1 || k.RPM != 0 || k.MonthlyTokenBudget != 0 || k.ExpiresAt != nil {
		t.Errorf("after update %+v", k)
	}
	if code, body, _ := c.get("/keys/tanaka"); code != 200 || !strings.Contains(body, `value="chat" checked`) {
		t.Errorf("edit page: %d", code)
	}

	// Revoke.
	if code, _, _ := c.post("/keys/tanaka/revoke", url.Values{"csrf": {c.csrf}}); code != 303 {
		t.Fatalf("revoke: %d", code)
	}
	if ms.keys[0].RevokedAt == nil {
		t.Error("not revoked")
	}
	if code, _, _ := c.get("/keys/tanaka"); code != 409 {
		t.Errorf("editing a revoked key: %d", code)
	}
	if code, _, _ := c.post("/keys/nobody/revoke", url.Values{"csrf": {c.csrf}}); code != 404 {
		t.Errorf("unknown key: %d", code)
	}
}

func TestUsagePage(t *testing.T) {
	c, _ := newAdmin(t)
	c.signIn()
	code, body, _ := c.get("/usage?month=2026-10")
	if code != 200 || !strings.Contains(body, "October 2026") || !strings.Contains(body, "1,234") {
		t.Errorf("usage: %d %s", code, body)
	}
	if code, _, _ := c.get("/usage?month=nope"); code != 400 {
		t.Errorf("bad month: %d", code)
	}
	if code, _, h := c.get("/static/app.css"); code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/css") {
		t.Errorf("static: %d %s", code, h.Get("Content-Type"))
	}
}

func TestComma(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1000: "-1,000"} {
		if got := comma(n); got != want {
			t.Errorf("comma(%d) = %q", n, got)
		}
	}
}
