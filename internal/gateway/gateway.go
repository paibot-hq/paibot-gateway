// Package gateway is a small OpenAI-compatible LLM gateway: it checks a
// virtual key, applies the key's model allowlist and rate limit, forwards
// the request to the configured upstreams with ordered fallback, and logs
// metadata about each request. Prompts and responses are never logged.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The request formats an upstream, and an endpoint, can speak.
const (
	apiOpenAI    = "openai"
	apiAnthropic = "anthropic"
)

// defaultAnthropicVersion is sent when an Anthropic client sends none.
const defaultAnthropicVersion = "2023-06-01"

type Gateway struct {
	log       *slog.Logger
	maxBody   int64
	keys      map[string]*virtualKey // by hash
	models    map[string][]target
	modelAPI  map[string]string
	handler   http.Handler
	now       func() time.Time
	upstreams map[string]*upstream
}

type virtualKey struct {
	name    string
	models  map[string]bool
	limiter *limiter
}

type upstream struct {
	name   string
	api    string
	base   *url.URL
	apiKey string
	client *http.Client
}

type target struct {
	up    *upstream
	model string
}

// New builds a gateway from a validated config. getenv resolves upstream
// API keys; a referenced variable that is unset or empty is an error, so a
// missing secret fails at startup instead of on the first request.
func New(cfg *Config, log *slog.Logger, getenv func(string) string) (*Gateway, error) {
	g := &Gateway{
		log:       log,
		maxBody:   cfg.MaxBodyBytes,
		keys:      map[string]*virtualKey{},
		models:    map[string][]target{},
		modelAPI:  map[string]string{},
		now:       time.Now,
		upstreams: map[string]*upstream{},
	}
	for name, uc := range cfg.Upstreams {
		base, err := url.Parse(uc.BaseURL)
		if err != nil {
			return nil, err
		}
		u := &upstream{name: name, api: uc.API, base: base, client: newClient(time.Duration(uc.TimeoutSeconds) * time.Second)}
		if uc.APIKeyEnv != "" {
			u.apiKey = getenv(uc.APIKeyEnv)
			if u.apiKey == "" {
				return nil, fmt.Errorf("upstream %q: environment variable %s is not set", name, uc.APIKeyEnv)
			}
		}
		g.upstreams[name] = u
	}
	for name, mc := range cfg.Models {
		for _, t := range mc.Targets {
			g.models[name] = append(g.models[name], target{up: g.upstreams[t.Upstream], model: t.Model})
		}
		g.modelAPI[name] = cfg.Upstreams[mc.Targets[0].Upstream].API
	}
	for _, kc := range cfg.Keys {
		k := &virtualKey{name: kc.Name, models: map[string]bool{}, limiter: newLimiter(kc.RPM)}
		for _, m := range kc.Models {
			k.models[m] = true
		}
		g.keys[kc.Hash] = k
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /v1/models", g.listModels)
	for _, p := range []string{"/chat/completions", "/completions", "/embeddings"} {
		mux.HandleFunc("POST /v1"+p, g.proxy(apiOpenAI, p))
	}
	for _, p := range []string{"/messages", "/messages/count_tokens"} {
		mux.HandleFunc("POST /v1"+p, g.proxy(apiAnthropic, p))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Unknown endpoint.")
	})
	g.handler = mux
	return g, nil
}

func newClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // never route through environment proxies
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
		},
		// An upstream redirect could point anywhere, including internal
		// addresses. Treat it as the final response instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type ctxKey int

const requestIDKey ctxKey = 0

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := newRequestID()
	h := w.Header()
	h.Set("X-Request-Id", id)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	g.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
}

func newRequestID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// authenticate reads the virtual key from "Authorization: Bearer", or
// failing that from "x-api-key", which Anthropic clients send.
func (g *Gateway) authenticate(r *http.Request) *virtualKey {
	var key string
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
			return nil
		}
		key = h[7:]
	} else {
		key = r.Header.Get("X-Api-Key")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	// Looking up the hash rather than the key itself means timing cannot
	// reveal how much of a guessed key was right.
	return g.keys[HashKey(key)]
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	k := g.authenticate(r)
	if k == nil {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key.")
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []model{}
	for m := range k.models {
		data = append(data, model{ID: m, Object: "model", OwnedBy: "paigate"})
	}
	slices.SortFunc(data, func(a, b model) int { return strings.Compare(a.ID, b.ID) })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// record is what gets logged for each proxied request: metadata only.
type record struct {
	key, model, upstream, targetModel string
	status, attempts                  int
	stream                            bool
	usage                             *usage
	err                               string
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// proxy serves one endpoint whose requests are in the format api.
func (g *Gateway) proxy(api, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := g.now()
		var rec record
		defer func() { g.logRecord(r, start, &rec) }()

		k := g.authenticate(r)
		if k == nil {
			rec.status = http.StatusUnauthorized
			writeAPIError(w, api, rec.status, "invalid_api_key", "Invalid API key.")
			return
		}
		rec.key = k.name

		if ok, wait := k.limiter.allow(start); !ok {
			rec.status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeAPIError(w, api, rec.status, "rate_limit_exceeded", "Rate limit exceeded for this key.")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				rec.status = http.StatusRequestEntityTooLarge
				writeAPIError(w, api, rec.status, "request_too_large", "Request body is too large.")
			} else {
				rec.status = http.StatusBadRequest
				writeAPIError(w, api, rec.status, "invalid_request", "Could not read request body.")
			}
			return
		}
		var req map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil || req == nil {
			rec.status = http.StatusBadRequest
			writeAPIError(w, api, rec.status, "invalid_request", "Request body must be a JSON object.")
			return
		}
		var model string
		if err := json.Unmarshal(req["model"], &model); err != nil || model == "" {
			rec.status = http.StatusBadRequest
			writeAPIError(w, api, rec.status, "invalid_request", "\"model\" must be a non-empty string.")
			return
		}
		rec.model = model
		// Same answer whether the model does not exist or the key may not
		// use it, so a key cannot be used to discover other models.
		if !k.models[model] {
			rec.status = http.StatusNotFound
			writeAPIError(w, api, rec.status, "model_not_found", fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
			return
		}
		if g.modelAPI[model] != api {
			rec.status = http.StatusBadRequest
			writeAPIError(w, api, rec.status, "invalid_request", fmt.Sprintf("The model %q is not served on this endpoint.", model))
			return
		}
		json.Unmarshal(req["stream"], &rec.stream)

		targets := g.models[model]
		for i, t := range targets {
			rec.attempts++
			rec.upstream, rec.targetModel = t.up.name, t.model
			req["model"], _ = json.Marshal(t.model)
			out, err := json.Marshal(req)
			if err != nil {
				rec.status = http.StatusInternalServerError
				writeAPIError(w, api, rec.status, "internal_error", "Could not encode request.")
				return
			}
			resp, err := t.up.send(r.Context(), suffix, out, r.Header)
			if err != nil {
				if r.Context().Err() != nil {
					rec.err = "client canceled"
					return
				}
				rec.err = err.Error()
				continue
			}
			if retryable(resp.StatusCode) && i < len(targets)-1 {
				rec.err = fmt.Sprintf("upstream %s returned %d", t.up.name, resp.StatusCode)
				io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				continue
			}
			rec.err = ""
			rec.status = resp.StatusCode
			rec.usage, err = relay(w, resp, api)
			if err != nil {
				rec.err = err.Error()
			}
			return
		}
		rec.status = http.StatusBadGateway
		writeAPIError(w, api, rec.status, "upstream_error", "No upstream could serve the request.")
	}
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529: // Anthropic's "overloaded"
		return true
	}
	return false
}

// send builds the upstream request from scratch. Nothing from the client's
// headers is forwarded except Accept and, to Anthropic upstreams, the
// anthropic-version and anthropic-beta headers: not its credentials,
// cookies or forwarding headers.
func (u *upstream) send(ctx context.Context, suffix string, body []byte, client http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.base.JoinPath(suffix).String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if accept := client.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	switch u.api {
	case apiAnthropic:
		version := client.Get("Anthropic-Version")
		if version == "" {
			version = defaultAnthropicVersion
		}
		req.Header.Set("Anthropic-Version", version)
		if betas := client.Values("Anthropic-Beta"); len(betas) > 0 {
			req.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
		}
		if u.apiKey != "" {
			req.Header.Set("X-Api-Key", u.apiKey)
		}
	default:
		if u.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+u.apiKey)
		}
	}
	return u.client.Do(req)
}

// maxCapture bounds how much of a non-streaming response is kept in memory
// to read token usage from.
const maxCapture = 4 << 20

// relay copies the upstream response to the client, flushing as it goes
// for event streams, and extracts token usage along the way.
func relay(w http.ResponseWriter, resp *http.Response, api string) (*usage, error) {
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	if strings.HasPrefix(ct, "text/event-stream") {
		rc := http.NewResponseController(w)
		sn := sseUsage{api: api}
		buf := make([]byte, 32<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				sn.Write(buf[:n])
				if _, err := w.Write(buf[:n]); err != nil {
					return sn.usage, err
				}
				if err := rc.Flush(); err != nil {
					return sn.usage, err
				}
			}
			if rerr == io.EOF {
				return sn.usage, nil
			}
			if rerr != nil {
				return sn.usage, rerr
			}
		}
	}

	var capture bytes.Buffer
	lw := &limitedWriter{w: &capture, n: maxCapture}
	if _, err := io.Copy(io.MultiWriter(w, lw), resp.Body); err != nil {
		return nil, err
	}
	if lw.overflow {
		return nil, nil
	}
	return parseUsage(api, capture.Bytes()), nil
}

// parseUsage reads token usage from a whole response body.
func parseUsage(api string, body []byte) *usage {
	if api == apiAnthropic {
		var ev anthropicEvent
		if json.Unmarshal(body, &ev) != nil {
			return nil
		}
		var a anthropicUsage
		a.merge(ev.Usage)
		return a.usage()
	}
	var parsed struct {
		Usage *usage `json:"usage"`
	}
	json.Unmarshal(body, &parsed)
	return parsed.Usage
}

// anthropicUsage is the token counts Anthropic reports. Pointers tell a
// missing field from a zero, since stream events carry only some fields.
type anthropicUsage struct {
	Input         *int `json:"input_tokens"`
	Output        *int `json:"output_tokens"`
	CacheCreation *int `json:"cache_creation_input_tokens"`
	CacheRead     *int `json:"cache_read_input_tokens"`
}

// merge overwrites fields present in b. Anthropic's stream counts are
// cumulative, so the latest value is the total.
func (a *anthropicUsage) merge(b *anthropicUsage) {
	if b == nil {
		return
	}
	for _, f := range [][2]**int{{&a.Input, &b.Input}, {&a.Output, &b.Output}, {&a.CacheCreation, &b.CacheCreation}, {&a.CacheRead, &b.CacheRead}} {
		if *f[1] != nil {
			*f[0] = *f[1]
		}
	}
}

// usage converts to OpenAI's terms, where prompt tokens include cached
// ones: Anthropic's input total is the sum of the three input fields.
func (a *anthropicUsage) usage() *usage {
	if a.Input == nil && a.Output == nil && a.CacheCreation == nil && a.CacheRead == nil {
		return nil
	}
	n := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	in := n(a.Input) + n(a.CacheCreation) + n(a.CacheRead)
	return &usage{PromptTokens: in, CompletionTokens: n(a.Output), TotalTokens: in + n(a.Output)}
}

// anthropicEvent covers a Messages response body and the stream events
// that carry usage: message_start nests it in "message", message_delta
// has it at the top.
type anthropicEvent struct {
	Usage   *anthropicUsage `json:"usage"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
}

type limitedWriter struct {
	w        io.Writer
	n        int
	overflow bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.overflow {
		return len(p), nil
	}
	if len(p) > l.n {
		l.overflow = true
		return len(p), nil
	}
	l.n -= len(p)
	return l.w.Write(p)
}

// sseUsage watches an event stream for a chunk carrying "usage", which
// OpenAI-compatible servers send last when asked to.
type sseUsage struct {
	api   string
	anth  anthropicUsage // running totals for Anthropic streams
	line  []byte
	skip  bool // current line exceeded maxLine and is being ignored
	usage *usage
}

const maxLine = 1 << 20

func (s *sseUsage) Write(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.append(p)
			return
		}
		s.append(p[:i])
		if !s.skip {
			s.parse(s.line)
		}
		s.line, s.skip = s.line[:0], false
		p = p[i+1:]
	}
}

func (s *sseUsage) append(p []byte) {
	if s.skip {
		return
	}
	if len(s.line)+len(p) > maxLine {
		s.skip = true
		return
	}
	s.line = append(s.line, p...)
}

func (s *sseUsage) parse(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	data = bytes.TrimSpace(data)
	if s.api == apiAnthropic {
		var ev anthropicEvent
		if json.Unmarshal(data, &ev) == nil {
			s.anth.merge(ev.Usage)
			if ev.Message != nil {
				s.anth.merge(ev.Message.Usage)
			}
			if u := s.anth.usage(); u != nil {
				s.usage = u
			}
		}
		return
	}
	var chunk struct {
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
		s.usage = chunk.Usage
	}
}

func (g *Gateway) logRecord(r *http.Request, start time.Time, rec *record) {
	id, _ := r.Context().Value(requestIDKey).(string)
	attrs := []slog.Attr{
		slog.String("request_id", id),
		slog.String("path", r.URL.Path),
		slog.String("key", rec.key),
		slog.String("model", rec.model),
		slog.String("upstream", rec.upstream),
		slog.String("target_model", rec.targetModel),
		slog.Int("status", rec.status),
		slog.Int("attempts", rec.attempts),
		slog.Bool("stream", rec.stream),
		slog.Int64("duration_ms", g.now().Sub(start).Milliseconds()),
	}
	if rec.usage != nil {
		attrs = append(attrs,
			slog.Int("prompt_tokens", rec.usage.PromptTokens),
			slog.Int("completion_tokens", rec.usage.CompletionTokens),
		)
	}
	if rec.err != "" {
		attrs = append(attrs, slog.String("error", rec.err))
	}
	g.log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError answers in the OpenAI error shape so existing SDKs surface it.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg,
		"type":    http.StatusText(status),
		"code":    code,
	}})
}

// anthropicErrorTypes maps statuses to the error types Anthropic's SDKs
// expect.
var anthropicErrorTypes = map[int]string{
	http.StatusBadRequest:            "invalid_request_error",
	http.StatusUnauthorized:          "authentication_error",
	http.StatusForbidden:             "permission_error",
	http.StatusNotFound:              "not_found_error",
	http.StatusRequestEntityTooLarge: "request_too_large",
	http.StatusTooManyRequests:       "rate_limit_error",
}

// writeAPIError answers in the error shape of the endpoint's format.
func writeAPIError(w http.ResponseWriter, api string, status int, code, msg string) {
	if api != apiAnthropic {
		writeError(w, status, code, msg)
		return
	}
	t, ok := anthropicErrorTypes[status]
	if !ok {
		t = "api_error"
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": t, "message": msg}})
}
