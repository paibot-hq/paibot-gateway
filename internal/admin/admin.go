// Package admin is a small web UI for managing database keys and viewing
// usage. One administrator signs in with a token; there are no user
// accounts or passwords to store. Pages are server-rendered with
// html/template and need no JavaScript beyond a copy button.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"paibot.app/paigate/internal/store"
)

// Store is what the UI needs from the database. *store.Store implements it.
type Store interface {
	Keys(ctx context.Context) ([]store.Key, error)
	Key(ctx context.Context, name string) (store.Key, error)
	CreateKey(ctx context.Context, name, hash string, set store.KeySettings) (store.Key, error)
	UpdateKey(ctx context.Context, name string, set store.KeySettings) error
	RevokeKey(ctx context.Context, name string) error
	TokensSince(ctx context.Context, t time.Time) (map[int64]int64, error)
	Report(ctx context.Context, from, to time.Time) ([]store.ReportRow, error)
}

type Options struct {
	Store Store
	// Token is the admin token. It must be at least 32 characters.
	Token string
	// Models lists the public model names keys may be given.
	Models   func() []string
	Location *time.Location
	Log      *slog.Logger
	// NewKey mints a virtual key and its hash.
	NewKey func() (key, hash string)
}

const (
	cookieName    = "paigate_admin"
	sessionTTL    = 12 * time.Hour
	minTokenChars = 32
)

type session struct {
	csrf    string
	expires time.Time
}

type Server struct {
	o         Options
	tokenHash [32]byte
	pages     map[string]*template.Template
	mux       *http.ServeMux
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	// Failed sign-ins draw from a bucket of 5 that refills one a minute.
	failTokens float64
	failLast   time.Time
}

//go:embed templates static
var files embed.FS

func New(o Options) (*Server, error) {
	if len(o.Token) < minTokenChars {
		return nil, fmt.Errorf("the admin token must be at least %d characters; `paigate genkey` makes a suitable one", minTokenChars)
	}
	s := &Server{o: o, tokenHash: sha256.Sum256([]byte(o.Token)), sessions: map[string]*session{}, now: time.Now, failTokens: 5}
	funcs := template.FuncMap{
		"comma": comma,
		"join":  strings.Join,
		"has":   slices.Contains[[]string],
		"step5": func(p int) int { return min(100, max(0, p/5*5)) },
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/base.html"))
	s.pages = map[string]*template.Template{}
	for _, p := range []string{"login", "keys", "key_form", "key_created", "usage", "error"} {
		s.pages[p] = template.Must(template.Must(base.Clone()).ParseFS(files, "templates/"+p+".html"))
	}

	static, _ := fs.Sub(files, "static")
	m := http.NewServeMux()
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("POST /logout", s.signedIn(s.logout))
	m.HandleFunc("GET /{$}", s.signedIn(s.keysPage))
	m.HandleFunc("GET /keys/new", s.signedIn(s.newKeyPage))
	m.HandleFunc("POST /keys/new", s.signedIn(s.createKey))
	m.HandleFunc("GET /keys/{name}", s.signedIn(s.editKeyPage))
	m.HandleFunc("POST /keys/{name}", s.signedIn(s.updateKey))
	m.HandleFunc("POST /keys/{name}/revoke", s.signedIn(s.revokeKey))
	m.HandleFunc("GET /usage", s.signedIn(s.usagePage))
	s.mux = m
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// ---- sessions

type ctxKey struct{}

// signedIn requires a valid session, and for POSTs a same-origin request
// carrying the session's CSRF token.
func (s *Server) signedIn(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "Your session has ended. Sign in again.", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "This form did not come from this page. Reload and try again.", http.StatusForbidden)
				return
			}
		}
		next(w, r, sess)
	}
}

func (s *Server) session(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[c.Value]
	if sess == nil || !s.now().Before(sess.expires) {
		delete(s.sessions, c.Value)
		return nil
	}
	return sess
}

// sameOrigin rejects cross-site form posts. Browsers send Sec-Fetch-Site
// on POSTs and a page cannot forge it, so it decides when present. Older
// browsers send only Origin. A request with neither did not come from a
// browser page on another site.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", nil, map[string]any{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Cross-site sign-in refused.", http.StatusForbidden)
		return
	}
	if !s.mayTry() {
		s.render(w, r, http.StatusTooManyRequests, "login", nil, map[string]any{"Error": "Too many failed attempts. Wait a minute and try again."})
		return
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(r.PostFormValue("token"))))
	if subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) != 1 {
		s.failed()
		s.o.Log.Warn("admin sign-in failed", "remote", r.RemoteAddr)
		s.render(w, r, http.StatusUnauthorized, "login", nil, map[string]any{"Error": "That token is not right."})
		return
	}
	id := randomHex(32)
	s.mu.Lock()
	for k, v := range s.sessions { // drop expired sessions
		if !s.now().Before(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = &session{csrf: randomHex(32), expires: s.now().Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	s.o.Log.Info("admin signed in", "remote", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) mayTry() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.failLast.IsZero() {
		s.failTokens = math.Min(5, s.failTokens+now.Sub(s.failLast).Minutes())
	}
	s.failLast = now
	return s.failTokens >= 1
}

func (s *Server) failed() {
	s.mu.Lock()
	s.failTokens--
	s.mu.Unlock()
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ *session) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- pages

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, sess *session, data map[string]any) {
	data["Page"] = page
	if sess != nil {
		data["CSRF"] = sess.csrf
		data["SignedIn"] = true
	}
	var b strings.Builder
	if err := s.pages[page].ExecuteTemplate(&b, "base", data); err != nil {
		s.o.Log.Error("admin template", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(b.String()))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, sess *session, err error) {
	s.o.Log.Error("admin request failed", "path", r.URL.Path, "error", err)
	s.render(w, r, http.StatusInternalServerError, "error", sess, map[string]any{"Error": "The database could not be reached or returned an error. Details are in the gateway log."})
}

type keyRow struct {
	store.Key
	Used    int64
	Percent int
	Expires string
	Status  string
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, sess *session) {
	ctx := r.Context()
	keys, err := s.o.Store.Keys(ctx)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	month := store.MonthStart(s.now(), s.o.Location)
	used, err := s.o.Store.TokensSince(ctx, month)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	now := s.now()
	rows := make([]keyRow, 0, len(keys))
	active := 0
	for _, k := range keys {
		row := keyRow{Key: k, Used: used[k.ID], Expires: store.FormatExpiry(k.ExpiresAt, s.o.Location)}
		switch {
		case k.RevokedAt != nil:
			row.Status = "revoked"
		case !k.Active(now):
			row.Status = "expired"
		default:
			row.Status = "active"
			active++
		}
		if k.MonthlyTokenBudget > 0 {
			row.Percent = int(min(100, row.Used*100/k.MonthlyTokenBudget))
		}
		rows = append(rows, row)
	}
	// Active keys first, then by name.
	slices.SortStableFunc(rows, func(a, b keyRow) int {
		return boolOrder(a.Status != "active", b.Status != "active")
	})
	s.render(w, r, http.StatusOK, "keys", sess, map[string]any{
		"Keys": rows, "Active": active, "Month": month.Format("January 2006"), "Notice": r.URL.Query().Get("notice"),
	})
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

type keyForm struct {
	Name, Owner, RPM, Budget, Expires string
	Models                            []string
}

func (s *Server) newKeyPage(w http.ResponseWriter, r *http.Request, sess *session) {
	s.render(w, r, http.StatusOK, "key_form", sess, map[string]any{"New": true, "Form": keyForm{}, "AllModels": s.o.Models()})
}

func (s *Server) editKeyPage(w http.ResponseWriter, r *http.Request, sess *session) {
	k, err := s.o.Store.Key(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	if k.RevokedAt != nil {
		s.render(w, r, http.StatusConflict, "error", sess, map[string]any{"Error": "This key has been revoked and can no longer be changed."})
		return
	}
	f := keyForm{Name: k.Name, Owner: k.Owner, Models: k.Models, Expires: store.FormatExpiry(k.ExpiresAt, s.o.Location)}
	if k.RPM > 0 {
		f.RPM = strconv.Itoa(k.RPM)
	}
	if k.MonthlyTokenBudget > 0 {
		f.Budget = strconv.FormatInt(k.MonthlyTokenBudget, 10)
	}
	s.render(w, r, http.StatusOK, "key_form", sess, map[string]any{"Form": f, "AllModels": s.o.Models()})
}

// settings reads and checks the key form.
func (s *Server) settings(r *http.Request) (keyForm, store.KeySettings, error) {
	f := keyForm{
		Name:    strings.TrimSpace(r.PostFormValue("name")),
		Owner:   strings.TrimSpace(r.PostFormValue("owner")),
		RPM:     strings.TrimSpace(r.PostFormValue("rpm")),
		Budget:  strings.TrimSpace(r.PostFormValue("budget")),
		Expires: strings.TrimSpace(r.PostFormValue("expires")),
		Models:  r.PostForm["models"],
	}
	set := store.KeySettings{Owner: f.Owner}
	allowed := s.o.Models()
	for _, m := range f.Models {
		if !slices.Contains(allowed, m) {
			return f, set, store.Input(fmt.Sprintf("unknown model %q", m))
		}
	}
	set.Models = f.Models
	if f.RPM != "" {
		n, err := strconv.Atoi(f.RPM)
		if err != nil || n < 0 {
			return f, set, store.Input("requests per minute must be a whole number, or blank for no limit")
		}
		set.RPM = n
	}
	var err error
	if set.MonthlyTokenBudget, err = store.ParseTokens(f.Budget); err != nil {
		return f, set, err
	}
	if set.ExpiresAt, err = store.ParseExpiry(f.Expires, s.o.Location); err != nil {
		return f, set, err
	}
	return f, set, set.Validate()
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, sess *session) {
	f, set, err := s.settings(r)
	if err == nil && !store.ValidName(f.Name) {
		err = store.Input("the name must be 1 to 64 letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	var key string
	if err == nil {
		var hash string
		key, hash = s.o.NewKey()
		_, err = s.o.Store.CreateKey(r.Context(), f.Name, hash, set)
	}
	if err != nil {
		if !isInputError(err) {
			s.fail(w, r, sess, err)
			return
		}
		s.render(w, r, http.StatusBadRequest, "key_form", sess, map[string]any{"New": true, "Form": f, "AllModels": s.o.Models(), "Error": err.Error()})
		return
	}
	s.o.Log.Info("admin created a key", "name", f.Name)
	// The key is shown on this response only; it is stored nowhere.
	s.render(w, r, http.StatusOK, "key_created", sess, map[string]any{"Name": f.Name, "Key": key, "Model": f.Models[0]})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request, sess *session) {
	name := r.PathValue("name")
	f, set, err := s.settings(r)
	f.Name = name
	if err == nil {
		err = s.o.Store.UpdateKey(r.Context(), name, set)
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if !isInputError(err) {
			s.fail(w, r, sess, err)
			return
		}
		s.render(w, r, http.StatusBadRequest, "key_form", sess, map[string]any{"Form": f, "AllModels": s.o.Models(), "Error": err.Error()})
		return
	}
	s.o.Log.Info("admin updated a key", "name", name)
	http.Redirect(w, r, "/?notice="+url.QueryEscape("Saved "+name+". Changes reach the gateway within a few seconds."), http.StatusSeeOther)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, sess *session) {
	name := r.PathValue("name")
	err := s.o.Store.RevokeKey(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	s.o.Log.Info("admin revoked a key", "name", name)
	http.Redirect(w, r, "/?notice="+url.QueryEscape("Revoked "+name+". It stops working within a few seconds."), http.StatusSeeOther)
}

// isInputError tells a problem with what was typed, which goes back to
// the form, from a database failure.
func isInputError(err error) bool {
	var ie *store.InputError
	return errors.As(err, &ie)
}

type usageRow struct {
	store.ReportRow
	Total int64
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request, sess *session) {
	loc := s.o.Location
	from := store.MonthStart(s.now(), loc)
	if m := r.URL.Query().Get("month"); m != "" {
		t, err := time.ParseInLocation("2006-01", m, loc)
		if err != nil {
			http.Error(w, "month must look like 2026-11", http.StatusBadRequest)
			return
		}
		from = t
	}
	to := from.AddDate(0, 1, 0)
	rep, err := s.o.Store.Report(r.Context(), from, to)
	if err != nil {
		s.fail(w, r, sess, err)
		return
	}
	rows := make([]usageRow, 0, len(rep))
	var total store.ReportRow
	for _, x := range rep {
		rows = append(rows, usageRow{x, x.PromptTokens + x.CompletionTokens})
		total.Requests += x.Requests
		total.PromptTokens += x.PromptTokens
		total.CompletionTokens += x.CompletionTokens
	}
	data := map[string]any{
		"Rows": rows, "Total": usageRow{total, total.PromptTokens + total.CompletionTokens},
		"Month": from.Format("January 2006"), "Prev": from.AddDate(0, -1, 0).Format("2006-01"),
		"Zone": loc.String(),
	}
	if to.Before(s.now()) {
		data["Next"] = to.Format("2006-01")
	}
	s.render(w, r, http.StatusOK, "usage", sess, data)
}

// comma formats 1234567 as 1,234,567.
func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
