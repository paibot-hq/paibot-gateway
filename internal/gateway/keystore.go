package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paibot-hq/paibot-gateway/internal/store"
)

// KeyStore is the database behind run-time keys and usage. *store.Store
// implements it.
type KeyStore interface {
	Migrate(ctx context.Context) error
	ActiveKeys(ctx context.Context) ([]store.Key, error)
	TokensSince(ctx context.Context, t time.Time) (map[int64]int64, error)
	AddUsage(ctx context.Context, rows []store.UsageRow) error
}

// SyncInterval is how often keys are reloaded from the database and usage
// is written to it. A revoked key stops working within about this long.
const SyncInterval = 5 * time.Second

// dbKeys holds what the database contributes, so that requests never wait
// on it: keys are read from memory, and usage is summed in memory and
// written in batches. While the database is unreachable the last keys
// loaded stay in use and usage piles up until it can be written.
type dbKeys struct {
	store KeyStore
	loc   *time.Location
	log   *slog.Logger
	keys  atomic.Pointer[map[string]*virtualKey] // by hash

	migrated bool
	lastErr  string

	mu      sync.Mutex
	pending map[usageKey]*usageCounts // not yet written
	month   time.Time                 // the month the token counts below are for
	// totals is tokens per key this month according to the database at the
	// last sync; unsynced is tokens recorded here since then. Their sum is
	// what the budget is checked against.
	totals   map[int64]int64
	unsynced map[int64]int64
	// written is tokens this month already written but not yet seen in a
	// fresh total, so they can be moved from unsynced to totals.
	written map[int64]int64
}

type usageKey struct {
	keyID int64
	model string
	hour  time.Time
}

type usageCounts struct {
	requests, prompt, completion int64
}

// UseDatabase makes the gateway serve keys from ks and track usage there.
// Call it before serving, then call SyncOnce and run Sync.
func (g *Gateway) UseDatabase(ks KeyStore, loc *time.Location) {
	d := &dbKeys{
		store:    ks,
		loc:      loc,
		log:      g.log,
		pending:  map[usageKey]*usageCounts{},
		totals:   map[int64]int64{},
		unsynced: map[int64]int64{},
		written:  map[int64]int64{},
	}
	empty := map[string]*virtualKey{}
	d.keys.Store(&empty)
	g.db = d
}

// SyncOnce migrates the schema if that has not succeeded yet, writes
// pending usage, and reloads keys and this month's totals. It must not run
// concurrently with Sync.
func (g *Gateway) SyncOnce(ctx context.Context) error {
	return g.db.sync(ctx, g.now())
}

// Sync calls SyncOnce every SyncInterval until ctx ends, logging failures
// once each and recoveries. Usage still pending at the end is written by
// Flush.
func (g *Gateway) Sync(ctx context.Context) {
	t := time.NewTicker(SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.db.report(g.SyncOnce(ctx))
		}
	}
}

// Flush writes pending usage, for use at shutdown.
func (g *Gateway) Flush(ctx context.Context) error {
	if g.db == nil {
		return nil
	}
	return g.db.flush(ctx)
}

func (d *dbKeys) report(err error) {
	switch {
	case err != nil && err.Error() != d.lastErr:
		d.log.Error("database sync failed; using the keys loaded last", "error", err)
		d.lastErr = err.Error()
	case err == nil && d.lastErr != "":
		d.log.Info("database sync recovered")
		d.lastErr = ""
	}
}

func (d *dbKeys) sync(ctx context.Context, now time.Time) error {
	if !d.migrated {
		if err := d.store.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		d.migrated = true
	}
	if err := d.flush(ctx); err != nil {
		return fmt.Errorf("write usage: %w", err)
	}
	keys, err := d.store.ActiveKeys(ctx)
	if err != nil {
		return fmt.Errorf("load keys: %w", err)
	}
	d.setKeys(keys)

	month := store.MonthStart(now, d.loc)
	totals, err := d.store.TokensSince(ctx, month)
	if err != nil {
		return fmt.Errorf("load usage: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollover(now)
	if d.month.Equal(month) {
		d.totals = totals
		for id, n := range d.written {
			if d.unsynced[id] -= n; d.unsynced[id] <= 0 {
				delete(d.unsynced, id)
			}
		}
		clear(d.written)
	}
	return nil
}

// setKeys replaces the database keys, keeping the rate-limit bucket of
// each key whose rpm did not change.
func (d *dbKeys) setKeys(keys []store.Key) {
	prev := map[int64]*virtualKey{}
	for _, k := range *d.keys.Load() {
		prev[k.id] = k
	}
	next := make(map[string]*virtualKey, len(keys))
	for _, sk := range keys {
		k := &virtualKey{id: sk.ID, name: sk.Name, models: map[string]bool{}, rpm: sk.RPM, budget: sk.MonthlyTokenBudget}
		if p := prev[sk.ID]; p != nil && p.rpm == sk.RPM {
			k.limiter = p.limiter
		} else {
			k.limiter = newLimiter(sk.RPM)
		}
		for _, m := range sk.Models {
			k.models[m] = true
		}
		if sk.ExpiresAt != nil {
			k.expires = *sk.ExpiresAt
		}
		next[sk.Hash] = k
	}
	d.keys.Store(&next)
}

func (d *dbKeys) lookup(hash string) *virtualKey {
	return (*d.keys.Load())[hash]
}

// rollover resets the monthly counts when a new month has begun. d.mu
// must be held.
func (d *dbKeys) rollover(now time.Time) {
	if m := store.MonthStart(now, d.loc); !m.Equal(d.month) {
		d.month = m
		clear(d.totals)
		clear(d.unsynced)
		clear(d.written)
	}
}

func (d *dbKeys) record(id int64, model string, at time.Time, u *usage) {
	hour := at.UTC().Truncate(time.Hour)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollover(at)
	k := usageKey{id, model, hour}
	c := d.pending[k]
	if c == nil {
		c = &usageCounts{}
		d.pending[k] = c
	}
	c.requests++
	if u != nil {
		c.prompt += int64(u.PromptTokens)
		c.completion += int64(u.CompletionTokens)
		if !hour.Before(d.month) {
			d.unsynced[id] += int64(u.PromptTokens + u.CompletionTokens)
		}
	}
}

func (d *dbKeys) tokensUsed(id int64, now time.Time) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollover(now)
	return d.totals[id] + d.unsynced[id]
}

// flush writes pending usage. On failure the counts go back to pending, to
// be tried again; if the write did land but its reply was lost, they will
// be counted twice, which errs on the side of the budget.
func (d *dbKeys) flush(ctx context.Context) error {
	d.mu.Lock()
	batch := d.pending
	d.pending = map[usageKey]*usageCounts{}
	d.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	rows := make([]store.UsageRow, 0, len(batch))
	for k, c := range batch {
		rows = append(rows, store.UsageRow{KeyID: k.keyID, Model: k.model, Hour: k.hour, Requests: c.requests, PromptTokens: c.prompt, CompletionTokens: c.completion})
	}
	err := d.store.AddUsage(ctx, rows)

	d.mu.Lock()
	defer d.mu.Unlock()
	for k, c := range batch {
		if err != nil {
			p := d.pending[k]
			if p == nil {
				p = &usageCounts{}
				d.pending[k] = p
			}
			p.requests += c.requests
			p.prompt += c.prompt
			p.completion += c.completion
		} else if !k.hour.Before(d.month) {
			d.written[k.keyID] += c.prompt + c.completion
		}
	}
	return err
}
