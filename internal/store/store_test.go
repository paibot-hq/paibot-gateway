package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// openTest connects to PG_TEST_DSN and starts from empty tables.
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, "DROP TABLE IF EXISTS paigate_usage, paigate_keys, paigate_schema_migrations"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

const hashA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTest(t)
	for range 3 {
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	exp := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	k, err := s.CreateKey(ctx, "tanaka", hashA, KeySettings{Owner: "tanaka@example.com", Models: []string{"chat", "claude"}, RPM: 60, MonthlyTokenBudget: 1_000_000, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	if k.ID == 0 || k.Owner != "tanaka@example.com" || len(k.Models) != 2 || k.RPM != 60 || k.MonthlyTokenBudget != 1_000_000 || !k.ExpiresAt.Equal(exp) || k.RevokedAt != nil {
		t.Fatalf("created %+v", k)
	}
	if time.Since(k.CreatedAt) > time.Minute {
		t.Errorf("created_at %v", k.CreatedAt)
	}
	if _, err := s.CreateKey(ctx, "tanaka", hashB, KeySettings{Models: []string{"chat"}}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}
	for _, bad := range []string{"", "-x", "a b", "日本", string(make([]byte, 65))} {
		if _, err := s.CreateKey(ctx, bad, hashB, KeySettings{Models: []string{"chat"}}); err == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
	if _, err := s.CreateKey(ctx, "nomodels", hashB, KeySettings{}); err == nil {
		t.Error("accepted a key with no models")
	}
	if _, err := s.CreateKey(ctx, "suzuki", hashB, KeySettings{Models: []string{"chat"}}); err != nil {
		t.Fatal(err)
	}

	// Update replaces settings; zero budget and nil expiry mean none.
	if err := s.UpdateKey(ctx, "tanaka", KeySettings{Owner: "t", Models: []string{"chat"}, RPM: 10}); err != nil {
		t.Fatal(err)
	}
	k, _ = s.Key(ctx, "tanaka")
	if k.Owner != "t" || len(k.Models) != 1 || k.RPM != 10 || k.MonthlyTokenBudget != 0 || k.ExpiresAt != nil {
		t.Errorf("updated %+v", k)
	}

	if err := s.RevokeKey(ctx, "suzuki"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, "suzuki"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: %v", err)
	}
	if err := s.UpdateKey(ctx, "suzuki", KeySettings{Models: []string{"chat"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating a revoked key: %v", err)
	}
	all, _ := s.Keys(ctx)
	active, _ := s.ActiveKeys(ctx)
	if len(all) != 2 || len(active) != 1 || active[0].Name != "tanaka" {
		t.Errorf("all %d, active %+v", len(all), active)
	}

	// An expired key is not active.
	past := time.Now().Add(-time.Hour)
	s.UpdateKey(ctx, "tanaka", KeySettings{Models: []string{"chat"}, ExpiresAt: &past})
	if active, _ := s.ActiveKeys(ctx); len(active) != 0 {
		t.Errorf("expired key is active: %+v", active)
	}
}

func TestUsage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.CreateKey(ctx, "a", hashA, KeySettings{Models: []string{"chat"}})
	b, _ := s.CreateKey(ctx, "b", hashB, KeySettings{Models: []string{"chat"}})
	h := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	lastMonth := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	rows := []UsageRow{
		{KeyID: a.ID, Model: "chat", Hour: h, Requests: 1, PromptTokens: 10, CompletionTokens: 5},
		{KeyID: a.ID, Model: "claude", Hour: h, Requests: 2, PromptTokens: 100, CompletionTokens: 50},
		{KeyID: b.ID, Model: "chat", Hour: lastMonth, Requests: 1, PromptTokens: 7, CompletionTokens: 7},
	}
	for range 2 { // adding the same increments twice accumulates
		if err := s.AddUsage(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := s.TokensSince(ctx, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if tokens[a.ID] != 330 || tokens[b.ID] != 0 {
		t.Errorf("tokens %v", tokens)
	}
	rep, err := s.Report(ctx, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) != 2 || rep[0].Model != "chat" || rep[0].Requests != 2 || rep[1].PromptTokens != 200 {
		t.Errorf("report %+v", rep)
	}

	// Many rows at once go through in chunks.
	var many []UsageRow
	for i := range 2500 {
		many = append(many, UsageRow{KeyID: b.ID, Model: "m", Hour: h.Add(time.Duration(i) * time.Hour), Requests: 1, PromptTokens: 1})
	}
	if err := s.AddUsage(ctx, many); err != nil {
		t.Fatal(err)
	}
	tokens, _ = s.TokensSince(ctx, h)
	if tokens[b.ID] != 2500 {
		t.Errorf("chunked insert: %d", tokens[b.ID])
	}
}

func TestMonthStart(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	got := MonthStart(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC), jst) // Nov 1, 01:00 JST
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, jst); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
