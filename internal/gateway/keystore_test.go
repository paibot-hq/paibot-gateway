package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paibot-hq/paibot-gateway/internal/store"
)

type fakeStore struct {
	mu         sync.Mutex
	keys       []store.Key
	usage      []store.UsageRow
	failWrites bool
}

func (f *fakeStore) Migrate(context.Context) error { return nil }

func (f *fakeStore) ActiveKeys(context.Context) ([]store.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Key(nil), f.keys...), nil
}

func (f *fakeStore) TokensSince(_ context.Context, t time.Time) (map[int64]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]int64{}
	for _, r := range f.usage {
		if !r.Hour.Before(t) {
			out[r.KeyID] += r.PromptTokens + r.CompletionTokens
		}
	}
	return out, nil
}

func (f *fakeStore) AddUsage(_ context.Context, rows []store.UsageRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrites {
		return errors.New("database down")
	}
	f.usage = append(f.usage, rows...)
	return nil
}

func (f *fakeStore) setKeys(keys ...store.Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeStore) sum(id int64) (requests, tokens int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.usage {
		if r.KeyID == id {
			requests += r.Requests
			tokens += r.PromptTokens + r.CompletionTokens
		}
	}
	return
}

const dbKey = "pg_database-key"

// dbGateway serves "chat" (answers use 3+5 tokens) with the given keys in
// a fake database, on a clock the test controls.
func dbGateway(t *testing.T, keys ...store.Key) (*Gateway, *fakeStore, *time.Time) {
	t.Helper()
	up := fakeUpstream(t, nil, okAnswer)
	g := newTestGateway(t, config(up.URL), nil)
	now := time.Date(2026, 11, 30, 23, 50, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	fs := &fakeStore{keys: keys}
	g.UseDatabase(fs, time.UTC)
	if err := g.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, fs, &now
}

func chat(g *Gateway, key string) int {
	return do(g, "POST", "/v1/chat/completions", key, `{"model":"chat"}`).Code
}

func TestDatabaseKeyLifecycle(t *testing.T) {
	k := store.Key{ID: 7, Name: "tanaka", Hash: HashKey(dbKey), Models: []string{"chat"}}
	g, fs, _ := dbGateway(t, k)
	ctx := context.Background()

	if c := chat(g, dbKey); c != 200 {
		t.Fatalf("database key: status %d", c)
	}
	if c := chat(g, testKey); c != 200 {
		t.Fatalf("config-file keys still work alongside: status %d", c)
	}
	// Model access comes from the database row.
	if rec := do(g, "POST", "/v1/chat/completions", dbKey, `{"model":"secret"}`); rec.Code != 404 {
		t.Errorf("model outside the key's list: %d", rec.Code)
	}
	if err := g.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if req, tok := fs.sum(7); req != 1 || tok != 8 {
		t.Errorf("usage written: %d requests, %d tokens; want 1, 8", req, tok)
	}

	// Revoked in the database: refused after the next sync.
	fs.setKeys()
	g.SyncOnce(ctx)
	if c := chat(g, dbKey); c != 401 {
		t.Errorf("revoked key: status %d", c)
	}
}

func TestDatabaseKeyExpiry(t *testing.T) {
	exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	g, _, now := dbGateway(t, store.Key{ID: 1, Name: "temp", Hash: HashKey(dbKey), Models: []string{"chat"}, ExpiresAt: &exp})
	if c := chat(g, dbKey); c != 200 {
		t.Fatalf("before expiry: %d", c)
	}
	*now = exp
	rec := do(g, "POST", "/v1/chat/completions", dbKey, `{"model":"chat"}`)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("at expiry: %d %s", rec.Code, rec.Body)
	}
	if rec := do(g, "GET", "/v1/models", dbKey, ""); rec.Code != 401 {
		t.Errorf("models list with an expired key: %d", rec.Code)
	}
}

func TestBudget(t *testing.T) {
	g, fs, now := dbGateway(t, store.Key{ID: 1, Name: "capped", Hash: HashKey(dbKey), Models: []string{"chat"}, MonthlyTokenBudget: 10})
	ctx := context.Background()

	// 8 tokens, then 16: the second request starts under the cap and may
	// run past it; the third is refused.
	for i, want := range []int{200, 200, 429} {
		if c := chat(g, dbKey); c != want {
			t.Fatalf("request %d: status %d, want %d", i, c, want)
		}
	}
	rec := do(g, "POST", "/v1/chat/completions", dbKey, `{"model":"chat"}`)
	if errCode(t, rec) != "insufficient_quota" {
		t.Errorf("code %q", errCode(t, rec))
	}
	// Still refused once the counts are in the database.
	g.SyncOnce(ctx)
	if c := chat(g, dbKey); c != 429 {
		t.Errorf("after sync: %d", c)
	}
	if _, tok := fs.sum(1); tok != 16 {
		t.Errorf("tokens written %d, want 16", tok)
	}
	// A new month starts afresh.
	*now = now.Add(15 * time.Minute) // 2026-12-01 00:05
	if c := chat(g, dbKey); c != 200 {
		t.Errorf("next month: %d", c)
	}
}

func TestBudgetCountsOtherReplicas(t *testing.T) {
	g, fs, now := dbGateway(t, store.Key{ID: 1, Name: "shared", Hash: HashKey(dbKey), Models: []string{"chat"}, MonthlyTokenBudget: 100})
	// Another replica has already written 100 tokens this month.
	fs.AddUsage(context.Background(), []store.UsageRow{{KeyID: 1, Model: "chat", Hour: now.Truncate(time.Hour), Requests: 5, PromptTokens: 100}})
	g.SyncOnce(context.Background())
	if c := chat(g, dbKey); c != 429 {
		t.Errorf("status %d, want 429", c)
	}
}

func TestUsageSurvivesWriteFailures(t *testing.T) {
	g, fs, _ := dbGateway(t, store.Key{ID: 3, Name: "k", Hash: HashKey(dbKey), Models: []string{"chat"}, MonthlyTokenBudget: 1000})
	ctx := context.Background()
	fs.failWrites = true
	chat(g, dbKey)
	chat(g, dbKey)
	if err := g.SyncOnce(ctx); err == nil {
		t.Fatal("sync should report the failed write")
	}
	if used := g.db.tokensUsed(3, g.now()); used != 16 {
		t.Errorf("budget count during the outage: %d, want 16", used)
	}
	chat(g, dbKey)
	fs.failWrites = false
	if err := g.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if req, tok := fs.sum(3); req != 3 || tok != 24 {
		t.Errorf("after recovery: %d requests, %d tokens; want 3, 24", req, tok)
	}
	// Counted once, not twice, now that the database has them.
	if used := g.db.tokensUsed(3, g.now()); used != 24 {
		t.Errorf("budget count after recovery: %d, want 24", used)
	}
	// Flush at shutdown writes what is pending.
	chat(g, dbKey)
	if err := g.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if req, _ := fs.sum(3); req != 4 {
		t.Errorf("after flush: %d requests", req)
	}
}

func TestSyncKeepsRateLimitState(t *testing.T) {
	g, _, _ := dbGateway(t, store.Key{ID: 1, Name: "slow", Hash: HashKey(dbKey), Models: []string{"chat"}, RPM: 2})
	chat(g, dbKey)
	chat(g, dbKey)
	g.SyncOnce(context.Background())
	if c := chat(g, dbKey); c != 429 {
		t.Errorf("a sync must not refill the bucket: status %d", c)
	}
}

func TestReload(t *testing.T) {
	up := fakeUpstream(t, nil, okAnswer)
	cfg := strings.Replace(config(up.URL), `"rpm": 1000`, `"rpm": 2`, 1)
	g := newTestGateway(t, cfg, nil)
	chat(g, testKey)
	chat(g, testKey)

	// The same key gains a model; its rpm is unchanged, so its bucket is kept.
	next, err := ParseConfig([]byte(strings.Replace(cfg, `"models": ["chat"]`, `"models": ["chat", "secret"]`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Reload(next); err != nil {
		t.Fatal(err)
	}
	if c := chat(g, testKey); c != 429 {
		t.Errorf("bucket was reset by the reload: status %d", c)
	}
	if rec := do(g, "GET", "/v1/models", testKey, ""); !strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("reloaded models not served: %s", rec.Body)
	}

	// A config that cannot be built leaves the running one in place.
	bad, _ := ParseConfig([]byte(`{"upstreams":{"a":{"base_url":"http://x","api_key_env":"MISSING"}}}`))
	if err := g.Reload(bad); err == nil {
		t.Error("reload with a missing secret succeeded")
	}
	if rec := do(g, "GET", "/v1/models", testKey, ""); rec.Code != 200 {
		t.Errorf("after a failed reload: %d", rec.Code)
	}
}
