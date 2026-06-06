// Command tatara-ingest walks a repository and pushes its code graph and
// semantic chunks to tatara-memory.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/config"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := realMain(); err != nil {
		slog.Error("ingest failed", "error", err)
		os.Exit(1)
	}
}

func realMain() error {
	cfg, err := config.Load(envLookup)
	if err != nil {
		return err
	}
	o := options{pollInterval: cfg.PollInterval, baseURL: cfg.BaseURL}
	fs := flag.NewFlagSet("tatara-ingest", flag.ContinueOnError)
	fs.StringVar(&o.repoRoot, "repo-root", "", "path to the repository root (required)")
	fs.StringVar(&o.repoName, "repo-name", "", "logical repo name (default: basename of repo-root)")
	fs.StringVar(&o.since, "since", "", "base commit for incremental ingest")
	fs.BoolVar(&o.full, "full", false, "force full re-ingest")
	fs.StringVar(&o.baseURL, "base-url", o.baseURL, "tatara-memory base URL")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if o.repoRoot == "" {
		return errMissingRepoRoot
	}
	if o.repoName == "" {
		o.repoName = filepath.Base(strings.TrimRight(o.repoRoot, "/"))
	}

	ctx := context.Background()
	hc := http.DefaultClient
	if cfg.OIDCClientID != "" {
		hc = push.OIDCClient(ctx, cfg.OIDCIssuer+"/protocol/openid-connect/token",
			cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCAudience, orDur(cfg.HTTPTimeout))
	}
	return run(ctx, o, hc)
}

func orDur(d time.Duration) time.Duration {
	if d <= 0 {
		return 60 * time.Second
	}
	return d
}

// envLookup maps a kebab-case config key to its UPPER_SNAKE env var.
func envLookup(key string) string {
	return os.Getenv(strings.ToUpper(strings.ReplaceAll(key, "-", "_")))
}
