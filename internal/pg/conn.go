// Package pg is a minimal PostgreSQL client on the standard library alone:
// TLS with certificate verification, SCRAM-SHA-256 authentication, and
// parameterized queries over the extended protocol, with results as text.
// It covers what a gateway needs and deliberately nothing more: no other
// authentication methods, no binary formats, no COPY or LISTEN.
package pg

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config says where and how to connect.
type Config struct {
	Host, Port, User, Password, Database string
	// TLS is nil only when the URL says sslmode=disable.
	TLS            *tls.Config
	ConnectTimeout time.Duration
	// ApplicationName shows in pg_stat_activity; "paigate" if empty.
	ApplicationName string
}

// ParseURL reads postgres://user:password@host:port/database?sslmode=...
// sslmode is verify-full (the default: encrypt and check the certificate
// and host name) or disable. The weaker modes are not offered, since they
// encrypt without knowing who is on the other end. sslrootcert names a PEM
// file of trusted roots; without it the system roots are used.
func ParseURL(s string) (*Config, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("pg: URL must start with postgres://")
	}
	c := &Config{Host: u.Hostname(), Port: u.Port(), Database: strings.TrimPrefix(u.Path, "/"), ConnectTimeout: 10 * time.Second}
	if c.Port == "" {
		c.Port = "5432"
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	if c.Host == "" || c.User == "" {
		return nil, errors.New("pg: URL needs a host and a user")
	}
	if c.Database == "" {
		c.Database = c.User
	}
	q := u.Query()
	for k := range q {
		if k != "sslmode" && k != "sslrootcert" && k != "connect_timeout" {
			return nil, fmt.Errorf("pg: unsupported URL parameter %q", k)
		}
	}
	if t := q.Get("connect_timeout"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return nil, errors.New("pg: connect_timeout must be a positive number of seconds")
		}
		c.ConnectTimeout = time.Duration(n) * time.Second
	}
	switch q.Get("sslmode") {
	case "disable":
		if q.Get("sslrootcert") != "" {
			return nil, errors.New("pg: sslrootcert given with sslmode=disable")
		}
	case "", "verify-full":
		c.TLS = &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
		if path := q.Get("sslrootcert"); path != "" {
			pem, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("pg: sslrootcert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("pg: sslrootcert holds no certificates")
			}
			c.TLS.RootCAs = pool
		}
	default:
		return nil, fmt.Errorf("pg: sslmode %q is not supported; use verify-full or disable", q.Get("sslmode"))
	}
	return c, nil
}

// Error is an error reported by the server.
type Error struct {
	Severity, Code, Message, Detail string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("pg: %s %s: %s", e.Severity, e.Code, e.Message)
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	return s
}

// ErrBroken is returned once a connection can no longer be trusted to be
// in step with the server, for instance after a timeout mid-query.
var ErrBroken = errors.New("pg: connection is broken")

const maxMessage = 64 << 20

// Conn is one connection. It is not safe for concurrent use.
type Conn struct {
	nc     net.Conn
	r      *bufio.Reader
	broken bool
}

// Connect opens and authenticates a connection.
func Connect(ctx context.Context, cfg *Config) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	nc, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return nil, err
	}
	stop := watch(ctx, nc)
	c, err := handshake(ctx, nc, cfg)
	stop()
	if err != nil {
		nc.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("pg: connect: %w", ctx.Err())
		}
		return nil, err
	}
	return c, nil
}

func handshake(ctx context.Context, nc net.Conn, cfg *Config) (*Conn, error) {
	if !printableASCII(cfg.Password) {
		// PostgreSQL normalizes non-ASCII passwords (SASLprep) before
		// hashing them, which needs Unicode tables the standard library
		// lacks. Fail clearly instead of with a puzzling auth error.
		return nil, errors.New("pg: the password must use printable ASCII characters only")
	}
	if cfg.TLS != nil {
		// SSLRequest. The one-byte answer is read straight from the socket,
		// with no buffering, so nothing sent before the TLS handshake can be
		// mistaken for data received inside it.
		var req [8]byte
		binary.BigEndian.PutUint32(req[0:], 8)
		binary.BigEndian.PutUint32(req[4:], 80877103)
		if _, err := nc.Write(req[:]); err != nil {
			return nil, err
		}
		var answer [1]byte
		if _, err := io.ReadFull(nc, answer[:]); err != nil {
			return nil, err
		}
		if answer[0] != 'S' {
			return nil, errors.New("pg: server does not accept TLS; refusing to connect without it")
		}
		tc := tls.Client(nc, cfg.TLS)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("pg: TLS: %w", err)
		}
		nc = tc
	}
	c := &Conn{nc: nc, r: bufio.NewReader(nc)}

	var b buffer
	b.int32(0) // length, filled in below
	b.int32(196608)
	app := cfg.ApplicationName
	if app == "" {
		app = "paigate"
	}
	for _, kv := range [][2]string{{"user", cfg.User}, {"database", cfg.Database}, {"application_name", app}, {"client_encoding", "UTF8"}} {
		b.cstring(kv[0])
		b.cstring(kv[1])
	}
	b.byte(0)
	binary.BigEndian.PutUint32(b[0:], uint32(len(b)))
	if _, err := nc.Write(b); err != nil {
		return nil, err
	}
	if err := c.authenticate(cfg); err != nil {
		return nil, err
	}
	for {
		typ, body, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch typ {
		case 'Z':
			return c, nil
		case 'E':
			return nil, parseError(body)
		}
		// ParameterStatus, BackendKeyData and notices need no action.
	}
}

func (c *Conn) authenticate(cfg *Config) error {
	var sc *scram
	for {
		typ, body, err := c.recv()
		if err != nil {
			return err
		}
		if typ == 'E' {
			return parseError(body)
		}
		if typ != 'R' || len(body) < 4 {
			return fmt.Errorf("pg: unexpected message %q during authentication", typ)
		}
		code, data := binary.BigEndian.Uint32(body), body[4:]
		switch {
		case code == 0: // AuthenticationOk
			if sc == nil {
				return errors.New("pg: server skipped authentication; refusing a server that does not check the password")
			}
			return nil
		case code == 10 && sc == nil: // AuthenticationSASL
			if !hasMechanism(data, "SCRAM-SHA-256") {
				return errors.New("pg: server does not offer SCRAM-SHA-256")
			}
			nonce := make([]byte, 18)
			rand.Read(nonce)
			sc = newSCRAM("", cfg.Password, base64.StdEncoding.EncodeToString(nonce))
			first := sc.clientFirst()
			var m buffer
			m.cstring("SCRAM-SHA-256")
			m.int32(int32(len(first)))
			m = append(m, first...)
			if err := c.send(message('p', m)); err != nil {
				return err
			}
		case code == 11 && sc != nil: // AuthenticationSASLContinue
			final, err := sc.clientFinal(string(data))
			if err != nil {
				return err
			}
			if err := c.send(message('p', []byte(final))); err != nil {
				return err
			}
		case code == 12 && sc != nil: // AuthenticationSASLFinal
			if err := sc.verifyServer(string(data)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pg: server asked for authentication method %d; only scram-sha-256 is supported", code)
		}
	}
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func hasMechanism(list []byte, want string) bool {
	for _, m := range strings.Split(string(list), "\x00") {
		if m == want {
			return true
		}
	}
	return false
}

// Rows is a whole result, read into memory. A nil value is SQL NULL.
type Rows struct {
	Columns []string
	Values  [][]*string
	// Tag is the command tag, such as "INSERT 0 1" or "SELECT 3".
	Tag string
}

// Query runs one statement with $1, $2, ... placeholders. Arguments are
// sent separately from the SQL, so they can never change its meaning.
// Supported argument types: nil, string, int, int32, int64, bool,
// time.Time.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	if c.broken {
		return nil, ErrBroken
	}
	params := make([][]byte, len(args))
	nulls := make([]bool, len(args))
	for i, a := range args {
		v, null, err := encode(a)
		if err != nil {
			return nil, fmt.Errorf("pg: argument $%d: %w", i+1, err)
		}
		params[i], nulls[i] = v, null
	}
	if len(args) > 65535 {
		return nil, errors.New("pg: too many arguments")
	}

	var parse, bind, describe, execute buffer
	parse.cstring("")
	parse.cstring(sql)
	parse.int16(0) // let the server infer parameter types
	bind.cstring("")
	bind.cstring("")
	bind.int16(0) // all parameters in text format
	bind.int16(int16(len(params)))
	for i, p := range params {
		if nulls[i] {
			bind.int32(-1)
			continue
		}
		bind.int32(int32(len(p)))
		bind = append(bind, p...)
	}
	bind.int16(0) // all results in text format
	describe.byte('P')
	describe.cstring("")
	execute.cstring("")
	execute.int32(0)
	var out []byte
	for _, m := range [][]byte{message('P', parse), message('B', bind), message('D', describe), message('E', execute), message('S', nil)} {
		out = append(out, m...)
	}

	stop := watch(ctx, c.nc)
	rows, err := c.exchange(out)
	stop()
	if err != nil {
		var pe *Error
		if !errors.As(err, &pe) {
			// An I/O failure leaves the protocol state unknown.
			c.broken = true
			c.nc.Close()
			if ctx.Err() != nil {
				return nil, fmt.Errorf("pg: query: %w", ctx.Err())
			}
		}
		return nil, err
	}
	return rows, nil
}

// exchange sends a batch of messages ending in Sync and reads up to
// ReadyForQuery. A server error is returned only after ReadyForQuery, so
// the connection stays usable.
func (c *Conn) exchange(out []byte) (*Rows, error) {
	if err := c.send(out); err != nil {
		return nil, err
	}
	rows := &Rows{}
	var serverErr error
	for {
		typ, body, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch typ {
		case 'T':
			if rows.Columns, err = parseRowDescription(body); err != nil {
				return nil, err
			}
		case 'D':
			row, err := parseDataRow(body)
			if err != nil {
				return nil, err
			}
			rows.Values = append(rows.Values, row)
		case 'C':
			rows.Tag, _, _ = strings.Cut(string(body), "\x00")
		case 'E':
			serverErr = parseError(body)
		case 'Z':
			if serverErr != nil {
				return nil, serverErr
			}
			return rows, nil
		}
		// ParseComplete, BindComplete, NoData, EmptyQueryResponse,
		// notices and parameter changes need no action.
	}
}

// watch makes reads and writes on nc fail once ctx ends. The returned
// function stops watching, waits for a watcher already firing, and clears
// the deadline, so a finished context never leaks into the next call.
func watch(ctx context.Context, nc net.Conn) (stop func()) {
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	fired := make(chan struct{})
	stopWatch := context.AfterFunc(ctx, func() {
		nc.SetDeadline(time.Now())
		close(fired)
	})
	return func() {
		if !stopWatch() {
			<-fired
		}
		nc.SetDeadline(time.Time{})
	}
}

// Exec runs a statement and returns its command tag.
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (string, error) {
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	return rows.Tag, nil
}

// Close ends the session politely and closes the socket.
func (c *Conn) Close() error {
	if !c.broken {
		c.send(message('X', nil))
	}
	c.broken = true
	return c.nc.Close()
}

func encode(v any) (b []byte, null bool, err error) {
	switch x := v.(type) {
	case nil:
		return nil, true, nil
	case string:
		if strings.IndexByte(x, 0) >= 0 {
			return nil, false, errors.New("text cannot contain NUL bytes")
		}
		return []byte(x), false, nil
	case int:
		return strconv.AppendInt(nil, int64(x), 10), false, nil
	case int32:
		return strconv.AppendInt(nil, int64(x), 10), false, nil
	case int64:
		return strconv.AppendInt(nil, x, 10), false, nil
	case bool:
		return strconv.AppendBool(nil, x), false, nil
	case time.Time:
		return []byte(x.UTC().Format("2006-01-02 15:04:05.999999Z07:00")), false, nil
	}
	return nil, false, fmt.Errorf("unsupported type %T", v)
}

// ---- wire format

type buffer []byte

func (b *buffer) byte(v byte)      { *b = append(*b, v) }
func (b *buffer) int16(v int16)    { *b = binary.BigEndian.AppendUint16(*b, uint16(v)) }
func (b *buffer) int32(v int32)    { *b = binary.BigEndian.AppendUint32(*b, uint32(v)) }
func (b *buffer) cstring(s string) { *b = append(append(*b, s...), 0) }

func message(typ byte, body []byte) []byte {
	m := make([]byte, 0, 5+len(body))
	m = append(m, typ)
	m = binary.BigEndian.AppendUint32(m, uint32(4+len(body)))
	return append(m, body...)
}

func (c *Conn) send(b []byte) error {
	_, err := c.nc.Write(b)
	return err
}

func (c *Conn) recv() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n < 4 || n > maxMessage {
		return 0, nil, fmt.Errorf("pg: message length %d out of range", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return h[0], body, nil
}

// reader walks a message body, recording the first overrun.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) int16() int {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return int(int16(binary.BigEndian.Uint16(v)))
}

func (r *reader) int32() int {
	v := r.take(4)
	if v == nil {
		return 0
	}
	return int(int32(binary.BigEndian.Uint32(v)))
}

func (r *reader) cstring() string {
	i := strings.IndexByte(string(r.b), 0)
	if r.bad || i < 0 {
		r.bad = true
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}

var errMalformed = errors.New("pg: malformed message from server")

func parseRowDescription(body []byte) ([]string, error) {
	r := &reader{b: body}
	n := r.int16()
	cols := make([]string, 0, max(n, 0))
	for range n {
		cols = append(cols, r.cstring())
		r.take(18) // table oid, column number, type oid, size, modifier, format
	}
	if r.bad {
		return nil, errMalformed
	}
	return cols, nil
}

func parseDataRow(body []byte) ([]*string, error) {
	r := &reader{b: body}
	n := r.int16()
	row := make([]*string, 0, max(n, 0))
	for range n {
		l := r.int32()
		if l == -1 {
			row = append(row, nil)
			continue
		}
		s := string(r.take(l))
		row = append(row, &s)
	}
	if r.bad {
		return nil, errMalformed
	}
	return row, nil
}

func parseError(body []byte) error {
	e := &Error{}
	for len(body) > 1 {
		code := body[0]
		v, rest, ok := strings.Cut(string(body[1:]), "\x00")
		if !ok {
			break
		}
		switch code {
		case 'S':
			e.Severity = v
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		case 'D':
			e.Detail = v
		}
		body = []byte(rest)
	}
	return e
}
