package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "pg_test-key"

type seen struct {
	auth, model, path, cookie string
}

// fakeUpstream records what it received and answers with the given handler.
func fakeUpstream(t *testing.T, got *seen, answer func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if got != nil {
			got.auth = r.Header.Get("Authorization")
			got.cookie = r.Header.Get("Cookie")
			got.path = r.URL.Path
			got.model, _ = body["model"].(string)
		}
		answer(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okAnswer(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"x","model":%q,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`, body["model"])
}

func newTestGateway(t *testing.T, cfgJSON string, logs *bytes.Buffer) *Gateway {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	g, err := New(cfg, slog.New(slog.NewJSONHandler(logs, nil)), func(name string) string {
		if name == "UP_KEY" {
			return "upstream-secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func config(urls ...string) string {
	ups, targets := []string{}, []string{}
	for i, u := range urls {
		ups = append(ups, fmt.Sprintf(`"up%d": {"base_url": %q, "api_key_env": "UP_KEY"}`, i, u+"/v1"))
		targets = append(targets, fmt.Sprintf(`{"upstream": "up%d", "model": "real-model-%d"}`, i, i))
	}
	return fmt.Sprintf(`{
		"upstreams": {%s},
		"models": {
			"chat": {"targets": [%s]},
			"secret": {"targets": [{"upstream": "up0", "model": "hidden"}]}
		},
		"keys": [{"name": "team-a", "hash": %q, "models": ["chat"], "rpm": 1000}]
	}`, strings.Join(ups, ","), strings.Join(targets, ","), HashKey(testKey))
}

func do(g *Gateway, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Cookie", "session=leak")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not an error object: %s", rec.Body)
	}
	return e.Error.Code
}

func TestProxyRewritesModelAndCredentials(t *testing.T) {
	var got seen
	up := fakeUpstream(t, &got, okAnswer)
	var logs bytes.Buffer
	g := newTestGateway(t, config(up.URL), &logs)

	rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat","messages":[{"role":"user","content":"top secret prompt"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got.model != "real-model-0" {
		t.Errorf("upstream got model %q, want real-model-0", got.model)
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("upstream path %q", got.path)
	}
	if got.auth != "Bearer upstream-secret" {
		t.Errorf("upstream Authorization %q: client key must be replaced by the upstream key", got.auth)
	}
	if got.cookie != "" {
		t.Errorf("client cookie was forwarded: %q", got.cookie)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id")
	}
	l := logs.String()
	if strings.Contains(l, "top secret prompt") || strings.Contains(l, testKey) {
		t.Errorf("log leaked prompt or key: %s", l)
	}
	if !strings.Contains(l, `"prompt_tokens":3`) || !strings.Contains(l, `"key":"team-a"`) {
		t.Errorf("log lacks usage metadata: %s", l)
	}
}

func TestAuthAndAllowlist(t *testing.T) {
	up := fakeUpstream(t, nil, okAnswer)
	g := newTestGateway(t, config(up.URL), nil)
	body := `{"model":"chat"}`

	for name, key := range map[string]string{"none": "", "wrong": "pg_wrong"} {
		if rec := do(g, "POST", "/v1/chat/completions", key, body); rec.Code != 401 || errCode(t, rec) != "invalid_api_key" {
			t.Errorf("%s key: status %d", name, rec.Code)
		}
	}
	// A model the key may not use and one that does not exist look the same.
	a := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"secret"}`)
	b := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"nope"}`)
	if a.Code != 404 || b.Code != 404 || errCode(t, a) != errCode(t, b) {
		t.Errorf("not-allowed %d / missing %d should both be 404 model_not_found", a.Code, b.Code)
	}
	if rec := do(g, "POST", "/v1/chat/completions", testKey, `not json`); rec.Code != 400 {
		t.Errorf("bad json: status %d", rec.Code)
	}
	if rec := do(g, "GET", "/v1/models", testKey, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"chat"`) || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("models list: %d %s", rec.Code, rec.Body)
	}
	if rec := do(g, "POST", "/v1/files", testKey, body); rec.Code != 404 {
		t.Errorf("unknown endpoint: status %d", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	up := fakeUpstream(t, nil, okAnswer)
	cfg := strings.Replace(config(up.URL), `"upstreams"`, `"max_body_bytes": 64, "upstreams"`, 1)
	g := newTestGateway(t, cfg, nil)
	big := `{"model":"chat","messages":"` + strings.Repeat("a", 100) + `"}`
	if rec := do(g, "POST", "/v1/chat/completions", testKey, big); rec.Code != 413 {
		t.Errorf("status %d, want 413", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	up := fakeUpstream(t, nil, okAnswer)
	g := newTestGateway(t, strings.Replace(config(up.URL), `"rpm": 1000`, `"rpm": 2`, 1), nil)
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	body := `{"model":"chat"}`

	for i := range 2 {
		if rec := do(g, "POST", "/v1/chat/completions", testKey, body); rec.Code != 200 {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	rec := do(g, "POST", "/v1/chat/completions", testKey, body)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("status %d Retry-After %q, want 429 and 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	now = now.Add(30 * time.Second)
	if rec := do(g, "POST", "/v1/chat/completions", testKey, body); rec.Code != 200 {
		t.Errorf("after refill: status %d", rec.Code)
	}
}

func TestFallback(t *testing.T) {
	var calls atomic.Int32
	bad := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		calls.Add(1)
		w.WriteHeader(503)
	})
	var got seen
	good := fakeUpstream(t, &got, okAnswer)
	g := newTestGateway(t, config(bad.URL, good.URL), nil)

	rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat"}`)
	if rec.Code != 200 || calls.Load() != 1 || got.model != "real-model-1" {
		t.Fatalf("status %d, bad calls %d, fallback model %q", rec.Code, calls.Load(), got.model)
	}

	// When every target fails, the last upstream's answer is passed on.
	g = newTestGateway(t, config(bad.URL, bad.URL), nil)
	if rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat"}`); rec.Code != 503 {
		t.Errorf("all failing: status %d, want 503", rec.Code)
	}

	// An unreachable upstream falls through to 502.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	g = newTestGateway(t, config(dead.URL), nil)
	if rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat"}`); rec.Code != 502 {
		t.Errorf("unreachable: status %d, want 502", rec.Code)
	}
}

func TestNoRedirectFollow(t *testing.T) {
	var hit atomic.Bool
	internal := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) { hit.Store(true) })
	redirector := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Location", internal.URL)
		w.WriteHeader(307)
	})
	g := newTestGateway(t, config(redirector.URL), nil)
	rec := do(g, "POST", "/v1/chat/completions", testKey, `{"model":"chat"}`)
	if hit.Load() || rec.Code != 307 {
		t.Errorf("redirect was followed (hit=%v, status %d)", hit.Load(), rec.Code)
	}
}

func TestStreaming(t *testing.T) {
	up := fakeUpstream(t, nil, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range []string{"hel", "lo"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			fl.Flush()
		}
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\ndata: [DONE]\n\n")
	})
	var logs bytes.Buffer
	g := newTestGateway(t, config(up.URL), &logs)
	srv := httptest.NewServer(g)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(string(b), `"hel"`) || !strings.HasSuffix(string(b), "data: [DONE]\n\n") {
		t.Fatalf("stream not relayed intact: %q", b)
	}
	srv.Close() // wait for the handler, and so the log line, to finish
	if l := logs.String(); !strings.Contains(l, `"completion_tokens":2`) || !strings.Contains(l, `"stream":true`) {
		t.Errorf("stream usage not logged: %s", l)
	}
}

func TestSSEUsageSplitAcrossWrites(t *testing.T) {
	var s sseUsage
	msg := "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":1,\"total_tokens\":8}}\r\n\r\n"
	for i := range len(msg) {
		s.Write([]byte{msg[i]})
	}
	if s.usage == nil || s.usage.PromptTokens != 7 {
		t.Fatalf("usage = %+v", s.usage)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]string{
		"unknown field":  `{"upstreams":{"a":{"base_url":"http://x"}},"typo":1}`,
		"no upstreams":   `{}`,
		"bad scheme":     `{"upstreams":{"a":{"base_url":"file:///etc"}}}`,
		"credentials":    `{"upstreams":{"a":{"base_url":"http://u:p@x"}}}`,
		"unknown target": `{"upstreams":{"a":{"base_url":"http://x"}},"models":{"m":{"targets":[{"upstream":"b","model":"m"}]}}}`,
		"bad hash":       `{"upstreams":{"a":{"base_url":"http://x"}},"models":{"m":{"targets":[{"upstream":"a","model":"m"}]}},"keys":[{"name":"k","hash":"plaintext","models":["m"]}]}`,
		"unknown model":  `{"upstreams":{"a":{"base_url":"http://x"}},"keys":[{"name":"k","hash":"` + HashKey("x") + `","models":["m"]}]}`,
		"trailing data":  `{"upstreams":{"a":{"base_url":"http://x"}}} {}`,
	}
	for name, c := range cases {
		if _, err := ParseConfig([]byte(c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg, err := ParseConfig([]byte(`{"upstreams":{"a":{"base_url":"http://x","api_key_env":"MISSING"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, slog.New(slog.DiscardHandler), func(string) string { return "" }); err == nil {
		t.Error("missing upstream key env was accepted")
	}
}

func TestGenerateKey(t *testing.T) {
	a, b := GenerateKey(), GenerateKey()
	if a == b || !strings.HasPrefix(a, keyPrefix) || len(a) != len(keyPrefix)+43 {
		t.Errorf("keys %q %q", a, b)
	}
	if !validHash(HashKey(a)) {
		t.Error("HashKey output fails validHash")
	}
}
