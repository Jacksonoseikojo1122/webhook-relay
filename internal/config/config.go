// Package config loads the relay's sources from a JSON file. Secrets can be
// written as "$ENV_VAR" so the file itself can be committed.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Providers the relay knows how to verify and de-duplicate.
const (
	Paystack    = "paystack"
	Flutterwave = "flutterwave"
	Stripe      = "stripe"
	Generic     = "generic"
)

// Source is one sender pointed at /in/{name}, and where its events go.
type Source struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Secret used to check the incoming signature. For Paystack this is your
	// secret key; for Stripe the endpoint signing secret (whsec_...); for
	// Flutterwave the "secret hash" set in the dashboard.
	Secret string `json:"secret"`
	// Destination is your app's endpoint.
	Destination string `json:"destination"`
	// DestinationSecret signs what the relay sends, so your app checks one
	// signature format whatever the provider.
	DestinationSecret string `json:"destination_secret"`
}

type Config struct {
	Sources     []Source      `json:"sources"`
	MaxAttempts int           `json:"max_attempts"`
	Timeout     time.Duration `json:"-"`
	TimeoutRaw  string        `json:"delivery_timeout"`
}

var envRef = regexp.MustCompile(`^\$([A-Z0-9_]+)$`)

func expand(s string) string {
	if m := envRef.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return os.Getenv(m[1])
	}
	return s
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse validates a config from bytes.
func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 12
	}
	c.Timeout = 15 * time.Second
	if c.TimeoutRaw != "" {
		d, err := time.ParseDuration(c.TimeoutRaw)
		if err != nil {
			return nil, fmt.Errorf("delivery_timeout: %w", err)
		}
		c.Timeout = d
	}
	seen := map[string]bool{}
	for i := range c.Sources {
		s := &c.Sources[i]
		s.Secret = expand(s.Secret)
		s.DestinationSecret = expand(s.DestinationSecret)
		s.Destination = expand(s.Destination)
		if !validName.MatchString(s.Name) {
			return nil, fmt.Errorf("source %q: name must be lowercase letters, digits and dashes", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("source %q appears twice", s.Name)
		}
		seen[s.Name] = true
		switch s.Provider {
		case Paystack, Flutterwave, Stripe:
			if s.Secret == "" {
				return nil, fmt.Errorf("source %q: %s needs a secret, or every request would be rejected", s.Name, s.Provider)
			}
		case Generic:
		default:
			return nil, fmt.Errorf("source %q: unknown provider %q", s.Name, s.Provider)
		}
		u, err := url.Parse(s.Destination)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("source %q: destination must be an http(s) URL", s.Name)
		}
		if s.DestinationSecret == "" {
			return nil, fmt.Errorf("source %q: destination_secret is required so your app can verify deliveries", s.Name)
		}
	}
	return &c, nil
}

// Find returns the source with the given name.
func (c *Config) Find(name string) (*Source, bool) {
	for i := range c.Sources {
		if c.Sources[i].Name == name {
			return &c.Sources[i], true
		}
	}
	return nil, false
}
