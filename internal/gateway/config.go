package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // so timezones work on images without /usr/share/zoneinfo
)

// Config is the whole gateway configuration. It is loaded at startup and
// again on SIGHUP; secrets never live in it, only the names of
// environment variables and hashes of virtual keys.
type Config struct {
	Listen       string                    `json:"listen"`
	MaxBodyBytes int64                     `json:"max_body_bytes"`
	Upstreams    map[string]UpstreamConfig `json:"upstreams"`
	Models       map[string]ModelConfig    `json:"models"`
	Keys         []KeyConfig               `json:"keys"`

	// DatabaseURLEnv names the environment variable holding a
	// postgres:// URL. With a database, keys can also be created and
	// revoked at run time, and usage and budgets are tracked.
	DatabaseURLEnv string `json:"database_url_env"`
	// AdminTokenEnv names the environment variable holding the admin
	// token. Setting it turns on the admin UI, which needs a database.
	AdminTokenEnv string `json:"admin_token_env"`
	// AdminListen is where the admin UI listens. The default only accepts
	// connections from the same machine.
	AdminListen string `json:"admin_listen"`
	// Timezone decides where calendar months, and so monthly budgets,
	// begin. An IANA name such as "Asia/Tokyo"; the default is UTC.
	Timezone string `json:"timezone"`
}

// UpstreamConfig is one OpenAI-compatible server, such as a vLLM instance.
type UpstreamConfig struct {
	BaseURL string `json:"base_url"`
	// APIKeyEnv names the environment variable holding the upstream's key.
	// Empty means the upstream needs no key (typical for in-cluster vLLM).
	APIKeyEnv string `json:"api_key_env"`
	// TimeoutSeconds bounds the wait for the response headers, not the
	// whole response, so long streams are not cut off.
	TimeoutSeconds int `json:"timeout_seconds"`
	// API is the request format the upstream speaks: "openai" (the
	// default) or "anthropic" (the Messages API).
	API string `json:"api"`
}

// ModelConfig maps a public model name to targets tried in order.
// Only the listed targets are ever used as fallbacks.
type ModelConfig struct {
	Targets []Target `json:"targets"`
}

type Target struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
}

// KeyConfig is one virtual key. Only its SHA-256 hash is stored.
type KeyConfig struct {
	Name   string   `json:"name"`
	Hash   string   `json:"hash"`
	Models []string `json:"models"`
	// RPM is the allowed requests per minute; 0 means unlimited.
	RPM int `json:"rpm"`
}

const (
	defaultListen       = ":8080"
	defaultAdminListen  = "127.0.0.1:8081"
	defaultMaxBodyBytes = 10 << 20
	defaultTimeout      = 120
)

// LoadConfig reads and validates a JSON config file. Unknown fields are
// rejected so that a typo cannot silently disable a restriction.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("config: trailing data after JSON object")
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.AdminListen == "" && c.AdminTokenEnv != "" {
		c.AdminListen = defaultAdminListen
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	for name, u := range c.Upstreams {
		if u.TimeoutSeconds == 0 {
			u.TimeoutSeconds = defaultTimeout
		}
		if u.API == "" {
			u.API = apiOpenAI
		}
		c.Upstreams[name] = u
	}
}

func (c *Config) Validate() error {
	var errs []error
	if c.MaxBodyBytes < 0 {
		errs = append(errs, errors.New("max_body_bytes must not be negative"))
	}
	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("at least one upstream is required"))
	}
	if c.AdminTokenEnv != "" && c.DatabaseURLEnv == "" {
		errs = append(errs, errors.New("admin_token_env needs database_url_env: the admin UI manages keys in the database"))
	}
	if c.AdminListen != "" && c.AdminTokenEnv == "" {
		errs = append(errs, errors.New("admin_listen is set but admin_token_env is not"))
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("timezone: %w", err))
	}
	for name, u := range c.Upstreams {
		b, err := url.Parse(u.BaseURL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("upstream %q: base_url: %w", name, err))
		case b.Scheme != "http" && b.Scheme != "https":
			errs = append(errs, fmt.Errorf("upstream %q: base_url must be http or https", name))
		case b.Host == "":
			errs = append(errs, fmt.Errorf("upstream %q: base_url has no host", name))
		case b.User != nil:
			errs = append(errs, fmt.Errorf("upstream %q: base_url must not contain credentials; use api_key_env", name))
		case b.RawQuery != "" || b.Fragment != "":
			errs = append(errs, fmt.Errorf("upstream %q: base_url must not have a query or fragment", name))
		}
		if u.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Errorf("upstream %q: timeout_seconds must not be negative", name))
		}
		if u.API != apiOpenAI && u.API != apiAnthropic {
			errs = append(errs, fmt.Errorf("upstream %q: api must be %q or %q", name, apiOpenAI, apiAnthropic))
		}
	}
	for name, m := range c.Models {
		if len(m.Targets) == 0 {
			errs = append(errs, fmt.Errorf("model %q: at least one target is required", name))
		}
		apis := map[string]bool{}
		for i, t := range m.Targets {
			if u, ok := c.Upstreams[t.Upstream]; !ok {
				errs = append(errs, fmt.Errorf("model %q: target %d: unknown upstream %q", name, i, t.Upstream))
			} else {
				apis[u.API] = true
			}
			if t.Model == "" {
				errs = append(errs, fmt.Errorf("model %q: target %d: model is required", name, i))
			}
		}
		// A request is forwarded as-is, never translated, so every target
		// of a model must speak the same format.
		if len(apis) > 1 {
			errs = append(errs, fmt.Errorf("model %q: targets mix upstreams with different api values", name))
		}
	}
	names := map[string]bool{}
	hashes := map[string]bool{}
	for i, k := range c.Keys {
		if k.Name == "" {
			errs = append(errs, fmt.Errorf("key %d: name is required", i))
		} else if names[k.Name] {
			errs = append(errs, fmt.Errorf("key %q: duplicate name", k.Name))
		}
		names[k.Name] = true
		if !validHash(k.Hash) {
			errs = append(errs, fmt.Errorf("key %q: hash must be \"sha256:\" followed by 64 lowercase hex digits", k.Name))
		} else if hashes[k.Hash] {
			errs = append(errs, fmt.Errorf("key %q: duplicate hash", k.Name))
		}
		hashes[k.Hash] = true
		if len(k.Models) == 0 {
			errs = append(errs, fmt.Errorf("key %q: at least one model is required", k.Name))
		}
		for _, m := range k.Models {
			if _, ok := c.Models[m]; !ok {
				errs = append(errs, fmt.Errorf("key %q: unknown model %q", k.Name, m))
			}
		}
		if k.RPM < 0 {
			errs = append(errs, fmt.Errorf("key %q: rpm must not be negative", k.Name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config:\n%w", err)
	}
	return nil
}

func validHash(h string) bool {
	hexPart, ok := strings.CutPrefix(h, hashPrefix)
	if !ok || len(hexPart) != 64 {
		return false
	}
	for _, r := range hexPart {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// Location is the configured timezone.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ModelNames lists the public model names, sorted.
func (c *Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for n := range c.Models {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
