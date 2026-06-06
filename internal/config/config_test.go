package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/config"
)

func TestLoadFromEnv(t *testing.T) {
	env := map[string]string{
		"base-url":           "https://memory.example/",
		"oidc-issuer":        "https://auth.example/realms/master",
		"oidc-client-id":     "ingester",
		"oidc-client-secret": "s3cret",
		"oidc-audience":      "tatara-memory",
		"poll-interval":      "2s",
		"http-timeout":       "30s",
	}
	c, err := config.Load(func(k string) string { return env[k] })
	require.NoError(t, err)
	require.Equal(t, "https://memory.example", c.BaseURL) // trailing slash trimmed
	require.Equal(t, "ingester", c.OIDCClientID)
	require.Equal(t, 2*time.Second, c.PollInterval)
	require.Equal(t, 30*time.Second, c.HTTPTimeout)
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(func(string) string { return "" })
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, c.PollInterval)
	require.Equal(t, 60*time.Second, c.HTTPTimeout)
}
