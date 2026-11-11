// Package store keeps virtual keys and hourly usage totals in PostgreSQL.
// It never stores prompts, responses, or individual requests: usage is a
// running sum per key, model and hour.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/paibot-hq/paibot-gateway/internal/pg"
)

// Key is a virtual key as stored. Only the hash of the key is kept.
type Key struct {
	ID     int64
	Name   string
	Owner  string
	Hash   string
	Models []string
	RPM    int
	// MonthlyTokenBudget caps prompt plus completion tokens per calendar
	// month; 0 means no cap.
	MonthlyTokenBudget int64
	ExpiresAt          *time.Time
	RevokedAt          *time.Time
	CreatedAt          time.Time
}

// Active reports whether the key may be used at t.
func (k *Key) Active(t time.Time) bool {
	return k.RevokedAt == nil && (k.ExpiresAt == nil || t.Before(*k.ExpiresAt))
}

// KeySettings are the parts of a key an administrator chooses.
type KeySettings struct {
	Owner              string
	Models             []string
	RPM                int
	MonthlyTokenBudget int64
	ExpiresAt          *time.Time
}

// UsageRow is one increment to the hourly totals.
type UsageRow struct {
	KeyID            int64
	Model            string
	Hour             time.Time
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
}

// ReportRow is a usage total for one key and model over a period.
type ReportRow struct {
	Key, Owner, Model                        string
	Requests, PromptTokens, CompletionTokens int64
}

// InputError is a problem with what was asked for, as opposed to a
// failure of the database; it is safe to show to whoever asked.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

// Input makes an InputError.
func Input(msg string) error { return &InputError{msg} }

func inputError(format string, args ...any) error {
	return &InputError{fmt.Sprintf(format, args...)}
}

var (
	ErrNotFound = errors.New("no such key")
	ErrExists   = &InputError{"a key with that name already exists"}
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName reports whether name can name a key: 1 to 64 letters, digits,
// dots, underscores or hyphens, starting with a letter or digit.
func ValidName(name string) bool { return validName.MatchString(name) }

// Validate checks settings before they are stored.
func (s *KeySettings) Validate() error {
	switch {
	case len(s.Owner) > 200:
		return inputError("owner must be at most 200 characters")
	case len(s.Models) == 0:
		return inputError("at least one model is required")
	case s.RPM < 0:
		return inputError("rpm must not be negative")
	case s.MonthlyTokenBudget < 0:
		return inputError("the monthly token budget must not be negative")
	}
	return nil
}

type Store struct {
	pool *pg.Pool
}

// Open connects lazily: the first query opens the first connection.
func Open(databaseURL string) (*Store, error) {
	cfg, err := pg.ParseURL(databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pg.NewPool(cfg, 4)}, nil
}

func (s *Store) Close() { s.pool.Close() }

var migrations = []string{
	// 1: keys and hourly usage.
	`CREATE TABLE paibot_gateway_keys (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		name text NOT NULL UNIQUE,
		owner text NOT NULL DEFAULT '',
		hash text NOT NULL UNIQUE CHECK (hash ~ '^sha256:[0-9a-f]{64}$'),
		models jsonb NOT NULL,
		rpm integer NOT NULL DEFAULT 0 CHECK (rpm >= 0),
		monthly_token_budget bigint CHECK (monthly_token_budget > 0),
		expires_at timestamptz,
		revoked_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now()
	);
	CREATE TABLE paibot_gateway_usage (
		key_id bigint NOT NULL REFERENCES paibot_gateway_keys,
		model text NOT NULL,
		hour timestamptz NOT NULL,
		requests bigint NOT NULL DEFAULT 0,
		prompt_tokens bigint NOT NULL DEFAULT 0,
		completion_tokens bigint NOT NULL DEFAULT 0,
		PRIMARY KEY (key_id, model, hour)
	);`,
}

const renameFromPaigate = `DO $$BEGIN
	IF to_regclass('paigate_schema_migrations') IS NOT NULL AND to_regclass('paibot_gateway_schema_migrations') IS NULL THEN
		ALTER TABLE paigate_schema_migrations RENAME TO paibot_gateway_schema_migrations;
		ALTER TABLE IF EXISTS paigate_keys RENAME TO paibot_gateway_keys;
		ALTER TABLE IF EXISTS paigate_usage RENAME TO paibot_gateway_usage;
	END IF;
END$$`

// Migrate brings the schema up to date. It is safe to run from several
// processes at once: they take turns under an advisory lock.
func (s *Store) Migrate(ctx context.Context) (err error) {
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer s.pool.Release(c)
	if _, err := c.Exec(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			c.Exec(context.Background(), "ROLLBACK")
		}
	}()
	if _, err := c.Exec(ctx, "SELECT pg_advisory_xact_lock(7268697320706169)"); err != nil {
		return err
	}
	// The project was called Paigate and its tables paigate_*; a database
	// made then takes the new names, data and all, before anything else.
	if _, err := c.Exec(ctx, renameFromPaigate); err != nil {
		return fmt.Errorf("renaming the paigate_* tables: %w", err)
	}
	if _, err := c.Exec(ctx, `CREATE TABLE IF NOT EXISTS paibot_gateway_schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	rows, err := c.Query(ctx, "SELECT coalesce(max(version), 0) FROM paibot_gateway_schema_migrations")
	if err != nil {
		return err
	}
	current, _ := strconv.Atoi(*rows.Values[0][0])
	if current > len(migrations) {
		return fmt.Errorf("the database schema is version %d, newer than this paibot-gateway knows (%d); upgrade paibot-gateway", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		// The extended protocol runs one statement at a time.
		for _, stmt := range strings.Split(migrations[v-1], ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := c.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := c.Exec(ctx, "INSERT INTO paibot_gateway_schema_migrations (version) VALUES ($1)", v); err != nil {
			return err
		}
	}
	_, err = c.Exec(ctx, "COMMIT")
	return err
}

// Timestamps travel as microseconds since the epoch, so their text form
// never depends on the session's DateStyle or TimeZone.
const keyColumns = `id, name, owner, hash, models::text, rpm, coalesce(monthly_token_budget, 0),
	(extract(epoch FROM expires_at) * 1000000)::bigint,
	(extract(epoch FROM revoked_at) * 1000000)::bigint,
	(extract(epoch FROM created_at) * 1000000)::bigint`

func scanKey(r []*string) (Key, error) {
	var k Key
	var err error
	num := func(p *string) int64 {
		if p == nil || err != nil {
			return 0
		}
		var n int64
		n, err = strconv.ParseInt(*p, 10, 64)
		return n
	}
	when := func(p *string) *time.Time {
		if p == nil {
			return nil
		}
		t := time.UnixMicro(num(p)).UTC()
		return &t
	}
	k.ID = num(r[0])
	k.Name, k.Owner, k.Hash = *r[1], *r[2], *r[3]
	if e := json.Unmarshal([]byte(*r[4]), &k.Models); e != nil {
		return k, fmt.Errorf("key %s: models: %w", k.Name, e)
	}
	k.RPM = int(num(r[5]))
	k.MonthlyTokenBudget = num(r[6])
	k.ExpiresAt, k.RevokedAt = when(r[7]), when(r[8])
	k.CreatedAt = *when(r[9])
	return k, err
}

func (s *Store) keys(ctx context.Context, where string, args ...any) ([]Key, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+keyColumns+" FROM paibot_gateway_keys "+where+" ORDER BY name", args...)
	if err != nil {
		return nil, err
	}
	keys := make([]Key, 0, len(rows.Values))
	for _, r := range rows.Values {
		k, err := scanKey(r)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// Keys returns every key, revoked and expired ones included.
func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	return s.keys(ctx, "")
}

// ActiveKeys returns the keys that may be used now.
func (s *Store) ActiveKeys(ctx context.Context) ([]Key, error) {
	return s.keys(ctx, "WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())")
}

func (s *Store) Key(ctx context.Context, name string) (Key, error) {
	keys, err := s.keys(ctx, "WHERE name = $1", name)
	if err != nil {
		return Key{}, err
	}
	if len(keys) == 0 {
		return Key{}, ErrNotFound
	}
	return keys[0], nil
}

func nullable(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func budget(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// CreateKey stores a new key under name with the given hash.
func (s *Store) CreateKey(ctx context.Context, name, hash string, set KeySettings) (Key, error) {
	if !ValidName(name) {
		return Key{}, inputError("the name must be 1 to 64 letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	if err := set.Validate(); err != nil {
		return Key{}, err
	}
	models, _ := json.Marshal(set.Models)
	_, err := s.pool.Exec(ctx, `INSERT INTO paibot_gateway_keys (name, owner, hash, models, rpm, monthly_token_budget, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)`,
		name, set.Owner, hash, string(models), set.RPM, budget(set.MonthlyTokenBudget), nullable(set.ExpiresAt))
	var pe *pg.Error
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Key{}, ErrExists
	}
	if err != nil {
		return Key{}, err
	}
	return s.Key(ctx, name)
}

// UpdateKey replaces the settings of a key that is not revoked.
func (s *Store) UpdateKey(ctx context.Context, name string, set KeySettings) error {
	if err := set.Validate(); err != nil {
		return err
	}
	models, _ := json.Marshal(set.Models)
	tag, err := s.pool.Exec(ctx, `UPDATE paibot_gateway_keys
		SET owner = $2, models = $3::jsonb, rpm = $4, monthly_token_budget = $5, expires_at = $6
		WHERE name = $1 AND revoked_at IS NULL`,
		name, set.Owner, string(models), set.RPM, budget(set.MonthlyTokenBudget), nullable(set.ExpiresAt))
	if err != nil {
		return err
	}
	if tag == "UPDATE 0" {
		return ErrNotFound
	}
	return nil
}

// RevokeKey stops a key for good. Its usage history is kept.
func (s *Store) RevokeKey(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, "UPDATE paibot_gateway_keys SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL", name)
	if err != nil {
		return err
	}
	if tag == "UPDATE 0" {
		return ErrNotFound
	}
	return nil
}

// AddUsage adds rows to the hourly totals in one statement per 1000 rows.
// Rows must not repeat a (key, model, hour).
func (s *Store) AddUsage(ctx context.Context, rows []UsageRow) error {
	for len(rows) > 0 {
		n := min(len(rows), 1000)
		var sb strings.Builder
		sb.WriteString("INSERT INTO paibot_gateway_usage (key_id, model, hour, requests, prompt_tokens, completion_tokens) VALUES ")
		args := make([]any, 0, n*6)
		for i, r := range rows[:n] {
			if i > 0 {
				sb.WriteString(",")
			}
			p := i * 6
			fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d)", p+1, p+2, p+3, p+4, p+5, p+6)
			args = append(args, r.KeyID, r.Model, r.Hour, r.Requests, r.PromptTokens, r.CompletionTokens)
		}
		sb.WriteString(` ON CONFLICT (key_id, model, hour) DO UPDATE SET
			requests = paibot_gateway_usage.requests + EXCLUDED.requests,
			prompt_tokens = paibot_gateway_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = paibot_gateway_usage.completion_tokens + EXCLUDED.completion_tokens`)
		if _, err := s.pool.Exec(ctx, sb.String(), args...); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

// TokensSince returns prompt plus completion tokens per key id since t.
func (s *Store) TokensSince(ctx context.Context, t time.Time) (map[int64]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT key_id, sum(prompt_tokens + completion_tokens)::bigint
		FROM paibot_gateway_usage WHERE hour >= $1 GROUP BY key_id`, t)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(rows.Values))
	for _, r := range rows.Values {
		id, _ := strconv.ParseInt(*r[0], 10, 64)
		n, _ := strconv.ParseInt(*r[1], 10, 64)
		out[id] = n
	}
	return out, nil
}

// Report sums usage per key and model for hours in [from, to).
func (s *Store) Report(ctx context.Context, from, to time.Time) ([]ReportRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT k.name, k.owner, u.model,
			sum(u.requests)::bigint, sum(u.prompt_tokens)::bigint, sum(u.completion_tokens)::bigint
		FROM paibot_gateway_usage u JOIN paibot_gateway_keys k ON k.id = u.key_id
		WHERE u.hour >= $1 AND u.hour < $2
		GROUP BY k.name, k.owner, u.model ORDER BY k.name, u.model`, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]ReportRow, 0, len(rows.Values))
	for _, r := range rows.Values {
		n := func(i int) int64 { v, _ := strconv.ParseInt(*r[i], 10, 64); return v }
		out = append(out, ReportRow{Key: *r[0], Owner: *r[1], Model: *r[2], Requests: n(3), PromptTokens: n(4), CompletionTokens: n(5)})
	}
	return out, nil
}

// MonthStart is the first instant of t's calendar month in loc.
func MonthStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

// ParseExpiry reads a date "2006-01-02" as "valid through that day" in
// loc, so the key expires when the next day begins. "" and "never" mean
// no expiry.
func ParseExpiry(date string, loc *time.Location) (*time.Time, error) {
	date = strings.TrimSpace(date)
	if date == "" || date == "never" {
		return nil, nil
	}
	d, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return nil, inputError("the expiry must be a date like 2027-03-31")
	}
	t := d.AddDate(0, 0, 1)
	return &t, nil
}

// FormatExpiry is the last day a key is valid, the inverse of ParseExpiry.
func FormatExpiry(t *time.Time, loc *time.Location) string {
	if t == nil {
		return ""
	}
	return t.Add(-time.Nanosecond).In(loc).Format("2006-01-02")
}

// ParseTokens reads a token count such as "1000000" or "1,000,000".
// "" means no budget.
func ParseTokens(s string) (int64, error) {
	s = strings.NewReplacer(",", "", "_", "", " ", "").Replace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, inputError("the budget must be a whole number of tokens")
	}
	return n, nil
}
