package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Setenv("PS_SECRET", "sk_from_env")
	c, err := Parse([]byte(`{
		"delivery_timeout": "5s",
		"sources": [{"name": "paystack-live", "provider": "paystack", "secret": "$PS_SECRET",
		             "destination": "https://app.example/hooks", "destination_secret": "d"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	src, ok := c.Find("paystack-live")
	if !ok || src.Secret != "sk_from_env" {
		t.Fatalf("env secret not expanded: %+v", src)
	}
	if c.MaxAttempts != 12 || c.Timeout != 5*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestParseRejectsMistakes(t *testing.T) {
	cases := map[string]string{
		"needs a secret":     `{"sources":[{"name":"a","provider":"stripe","destination":"https://x","destination_secret":"d"}]}`,
		"unknown provider":   `{"sources":[{"name":"a","provider":"mpesa","destination":"https://x","destination_secret":"d"}]}`,
		"http(s) URL":        `{"sources":[{"name":"a","provider":"generic","destination":"ftp://x","destination_secret":"d"}]}`,
		"destination_secret": `{"sources":[{"name":"a","provider":"generic","destination":"https://x"}]}`,
		"lowercase":          `{"sources":[{"name":"Bad Name","provider":"generic","destination":"https://x","destination_secret":"d"}]}`,
		"appears twice":      `{"sources":[{"name":"a","provider":"generic","destination":"https://x","destination_secret":"d"},{"name":"a","provider":"generic","destination":"https://x","destination_secret":"d"}]}`,
	}
	for want, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected error containing %q, got %v", want, err)
		}
	}
}
