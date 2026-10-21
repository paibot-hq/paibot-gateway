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

type Gateway struct {
	log       *slog.Logger
	maxBody   int64
	keys      map[string]*virtualKey // by hash
	models    map[string][]target
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
		now:       time.Now,
		upstreams: map[string]*upstream{},
	}
	for name, uc := range cfg.Upstreams {
		base, err := url.Parse(uc.BaseURL)
		if err != nil {
			return nil, err
		}
		u := &upstream{name: name, base: base, client: newClient(time.Duration(uc.TimeoutSeconds) * time.Second)}
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
		mux.HandleFunc("POST /v1"+p, g.proxy(p))
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

func (g *Gateway) authenticate(r *http.Request) *virtualKey {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return nil
	}
	// Looking up the hash rather than the key itself means timing cannot
	// reveal how much of a guessed key was right.
	return g.keys[HashKey(strings.TrimSpace(h[7:]))]
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

func (g *Gateway) proxy(suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := g.now()
		var rec record
		defer func() { g.logRecord(r, start, &rec) }()

		k := g.authenticate(r)
		if k == nil {
			rec.status = http.StatusUnauthorized
			writeError(w, rec.status, "invalid_api_key", "Invalid API key.")
			return
		}
		rec.key = k.name

		if ok, wait := k.limiter.allow(start); !ok {
			rec.status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, rec.status, "rate_limit_exceeded", "Rate limit exceeded for this key.")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				rec.status = http.StatusRequestEntityTooLarge
				writeError(w, rec.status, "request_too_large", "Request body is too large.")
			} else {
				rec.status = http.StatusBadRequest
				writeError(w, rec.status, "invalid_request", "Could not read request body.")
			}
			return
		}
		var req map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil || req == nil {
			rec.status = http.StatusBadRequest
			writeError(w, rec.status, "invalid_request", "Request body must be a JSON object.")
			return
		}
		var model string
		if err := json.Unmarshal(req["model"], &model); err != nil || model == "" {
			rec.status = http.StatusBadRequest
			writeError(w, rec.status, "invalid_request", "\"model\" must be a non-empty string.")
			return
		}
		rec.model = model
		// Same answer whether the model does not exist or the key may not
		// use it, so a key cannot be used to discover other models.
		if !k.models[model] {
			rec.status = http.StatusNotFound
			writeError(w, rec.status, "model_not_found", fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
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
				writeError(w, rec.status, "internal_error", "Could not encode request.")
				return
			}
			resp, err := t.up.send(r.Context(), suffix, out, r.Header.Get("Accept"))
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
			rec.usage, err = relay(w, resp)
			if err != nil {
				rec.err = err.Error()
			}
			return
		}
		rec.status = http.StatusBadGateway
		writeError(w, rec.status, "upstream_error", "No upstream could serve the request.")
	}
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// send builds the upstream request from scratch. Nothing from the client's
// headers is forwarded except Accept: not its Authorization, cookies or
// forwarding headers.
func (u *upstream) send(ctx context.Context, suffix string, body []byte, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.base.JoinPath(suffix).String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if u.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.apiKey)
	}
	return u.client.Do(req)
}

// maxCapture bounds how much of a non-streaming response is kept in memory
// to read token usage from.
const maxCapture = 4 << 20

// relay copies the upstream response to the client, flushing as it goes
// for event streams, and extracts token usage along the way.
func relay(w http.ResponseWriter, resp *http.Response) (*usage, error) {
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	if strings.HasPrefix(ct, "text/event-stream") {
		rc := http.NewResponseController(w)
		var sn sseUsage
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
	var parsed struct {
		Usage *usage `json:"usage"`
	}
	json.Unmarshal(capture.Bytes(), &parsed)
	return parsed.Usage, nil
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
	var chunk struct {
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &chunk) == nil && chunk.Usage != nil {
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
