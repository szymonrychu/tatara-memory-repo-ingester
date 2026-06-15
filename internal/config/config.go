// Package config loads ingester configuration from the environment.
package config

import (
	"fmt"
	"strings"
	"time"
)

// Config holds runtime configuration for the ingester.
type Config struct {
	BaseURL          string
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCAudience     string
	PollInterval     time.Duration
	HTTPTimeout      time.Duration
	CrossRepoPrefix  string
	MetricsPushURL   string
}

// Load builds a Config from a key lookup function (kebab-case keys).
func Load(getenv func(string) string) (Config, error) {
	crossRepoPrefix := getenv("cross-repo-prefix")
	if crossRepoPrefix == "" {
		crossRepoPrefix = "github.com/szymonrychu/"
	}
	c := Config{
		BaseURL:          strings.TrimRight(getenv("base-url"), "/"),
		OIDCIssuer:       getenv("oidc-issuer"),
		OIDCClientID:     getenv("oidc-client-id"),
		OIDCClientSecret: getenv("oidc-client-secret"),
		OIDCAudience:     getenv("oidc-audience"),
		PollInterval:     2 * time.Second,
		HTTPTimeout:      60 * time.Second,
		CrossRepoPrefix:  crossRepoPrefix,
		MetricsPushURL:   strings.TrimRight(getenv("metrics-push-url"), "/"),
	}
	if v := getenv("poll-interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("parse poll-interval: %w", err)
		}
		c.PollInterval = d
	}
	if v := getenv("http-timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("parse http-timeout: %w", err)
		}
		c.HTTPTimeout = d
	}
	return c, nil
}
