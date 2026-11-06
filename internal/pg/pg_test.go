package pg

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// RFC 7677 section 3 test vector.
func TestSCRAMVector(t *testing.T) {
	s := newSCRAM("user", "pencil", "rOprNGfwEbeRWgbNEkqO")
	if got := s.clientFirst(); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first %q", got)
	}
	final, err := s.clientFinal("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if err != nil {
		t.Fatal(err)
	}
	if want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="; final != want {
		t.Fatalf("client-final\n got %s\nwant %s", final, want)
	}
	if err := s.verifyServer("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyServer("v=AAAATRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="); err == nil {
		t.Fatal("accepted a wrong server signature")
	}
}

func TestSCRAMRejectsWeakChallenges(t *testing.T) {
	for name, challenge := range map[string]string{
		"low iterations": "r=abcXYZ,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=1",
		"nonce not ours": "r=zzzXYZ,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"same nonce":     "r=abc,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"mandatory ext":  "m=x,r=abcXYZ,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"bad salt":       "r=abcXYZ,s=!!!,i=4096",
	} {
		if _, err := newSCRAM("", "pw", "abc").clientFinal(challenge); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseURL(t *testing.T) {
	c, err := ParseURL("postgres://app:p%40ss@db.internal:6543/paigate?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db.internal" || c.Port != "6543" || c.User != "app" || c.Password != "p@ss" || c.Database != "paigate" || c.TLS == nil || c.TLS.ServerName != "db.internal" {
		t.Errorf("parsed %+v", c)
	}
	c, _ = ParseURL("postgres://app@localhost")
	if c.TLS == nil || c.Port != "5432" || c.Database != "app" {
		t.Errorf("defaults: TLS must default on: %+v", c)
	}
	for _, bad := range []string{
		"mysql://a@b", "postgres://b/db", "postgres://a@b?sslmode=require", "postgres://a@b?sslmode=prefer",
		"postgres://a@b?sslmode=disable&sslrootcert=x", "postgres://a@b?options=x", "postgres://a@b?sslrootcert=/nonexistent",
	} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// fakeServer runs script against the first connection.
func fakeServer(t *testing.T, script func(c net.Conn)) *Config {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		script(c)
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	return &Config{Host: host, Port: port, User: "u", Password: "pw", Database: "d", ConnectTimeout: 2 * time.Second}
}

func readStartup(c net.Conn) {
	var n [4]byte
	io.ReadFull(c, n[:])
	io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:]))-4)
}

func auth(code uint32, data string) []byte {
	b := binary.BigEndian.AppendUint32(nil, code)
	return message('R', append(b, data...))
}

func TestRefusesServerWithoutTLS(t *testing.T) {
	cfg := fakeServer(t, func(c net.Conn) {
		readStartup(c) // the SSLRequest
		c.Write([]byte{'N'})
	})
	cfg.TLS = tlsConfigFor(cfg.Host)
	if _, err := Connect(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesServerThatSkipsAuth(t *testing.T) {
	cfg := fakeServer(t, func(c net.Conn) {
		readStartup(c)
		c.Write(auth(0, "")) // AuthenticationOk without asking for a password
	})
	if _, err := Connect(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "skipped authentication") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesWeakAuthMethods(t *testing.T) {
	for _, code := range []uint32{3, 5} { // cleartext, md5
		cfg := fakeServer(t, func(c net.Conn) {
			readStartup(c)
			c.Write(auth(code, "salt"))
		})
		if _, err := Connect(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "only scram-sha-256") {
			t.Errorf("method %d: err = %v", code, err)
		}
	}
}

func TestConnectTimeout(t *testing.T) {
	cfg := fakeServer(t, func(c net.Conn) { time.Sleep(3 * time.Second) })
	cfg.ConnectTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err := Connect(context.Background(), cfg)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestMalformedMessagesDoNotPanic(t *testing.T) {
	for _, b := range [][]byte{nil, {0}, {0, 1}, {0, 1, 'x'}, {0, 1, 0, 0, 0}, {0, 2, 0, 0, 0, 5, 'a'}, {0xff, 0xff}} {
		parseRowDescription(b)
		parseDataRow(b)
		parseError(b)
	}
}

// ---- against a real server: set PG_TEST_DSN (and PG_TEST_TLS_DSN) to run.

func connectEnv(t *testing.T, name string) *Conn {
	t.Helper()
	dsn := os.Getenv(name)
	if dsn == "" {
		t.Skipf("%s not set", name)
	}
	cfg, err := ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPostgres(t *testing.T) {
	c := connectEnv(t, "PG_TEST_DSN")
	ctx := context.Background()
	if _, err := c.Exec(ctx, `CREATE TEMP TABLE keys (name text, hash text, rpm bigint, active boolean, expires timestamptz, note text)`); err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	evil := `x'); DROP TABLE keys; --`
	tag, err := c.Exec(ctx, `INSERT INTO keys VALUES ($1, $2, $3, $4, $5, $6)`, "チームA", evil, int64(60), true, exp, nil)
	if err != nil || tag != "INSERT 0 1" {
		t.Fatalf("insert: %q %v", tag, err)
	}
	rows, err := c.Query(ctx, `SELECT name, hash, rpm, active, expires = $1, note FROM keys WHERE rpm > $2`, exp, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Values) != 1 || rows.Tag != "SELECT 1" || strings.Join(rows.Columns, ",") != "name,hash,rpm,active,?column?,note" {
		t.Fatalf("rows %+v", rows)
	}
	r := rows.Values[0]
	if *r[0] != "チームA" || *r[1] != evil || *r[2] != "60" || *r[3] != "t" || *r[4] != "t" || r[5] != nil {
		t.Errorf("row %q %q %q %q %q %v", *r[0], *r[1], *r[2], *r[3], *r[4], r[5])
	}

	// A server error leaves the connection usable.
	_, err = c.Query(ctx, `SELEC 1`)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "42601" {
		t.Fatalf("syntax error: %v", err)
	}
	if rows, err := c.Query(ctx, `SELECT count(*) FROM keys`); err != nil || *rows.Values[0][0] != "1" {
		t.Fatalf("after error: %v", err)
	}

	// A timeout mid-query breaks the connection rather than desyncing it.
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Query(tctx, `SELECT pg_sleep(5)`); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
	if _, err := c.Query(ctx, `SELECT 1`); !errors.Is(err, ErrBroken) {
		t.Fatalf("after timeout: %v", err)
	}
}

func TestPostgresWrongPassword(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	cfg, _ := ParseURL(dsn)
	cfg.Password = "wrong"
	_, err := Connect(context.Background(), cfg)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "28P01" {
		t.Fatalf("err = %v", err)
	}
}

func TestPostgresTLS(t *testing.T) {
	c := connectEnv(t, "PG_TEST_TLS_DSN")
	rows, err := c.Query(context.Background(), `SELECT ssl, version FROM pg_stat_ssl WHERE pid = pg_backend_pid()`)
	if err != nil {
		t.Fatal(err)
	}
	if *rows.Values[0][0] != "t" {
		t.Fatalf("not encrypted: %+v", rows.Values)
	}
	t.Logf("TLS version: %s", *rows.Values[0][1])

	// The same server under a name its certificate does not cover is refused.
	if dsn := os.Getenv("PG_TEST_TLS_WRONGNAME_DSN"); dsn != "" {
		cfg, err := ParseURL(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Connect(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("wrong host name: err = %v", err)
		}
	}
}

func tlsConfigFor(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}
