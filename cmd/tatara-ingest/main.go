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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
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
	o, err := resolveOptions(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	if o.baseURL == "" {
		o.baseURL = cfg.BaseURL
	}
	o.pollInterval = cfg.PollInterval
	o.crossRepoPrefix = cfg.CrossRepoPrefix

	ctx := context.Background()
	hc := http.DefaultClient
	if cfg.OIDCClientID != "" {
		hc = push.OIDCClient(ctx, cfg.OIDCIssuer+"/protocol/openid-connect/token",
			cfg.OIDCClientID, cfg.OIDCClientSecret, cfg.OIDCAudience, orDur(cfg.HTTPTimeout))
	}
	return run(ctx, o, hc)
}

// resolveOptions parses flags and env vars to produce a populated options.
// getenv is injectable for testing. Env keys are the UPPER_SNAKE equivalent of
// the kebab flag names (REPO_ROOT, REPO_NAME). Flags override env. The
// basename fallback applies when repo-name is still empty after both. Returns
// errMissingRepoRoot when repo-root is unresolvable.
func resolveOptions(args []string, getenv func(string) string) (options, error) {
	o := options{}
	fs := flag.NewFlagSet("tatara-ingest", flag.ContinueOnError)
	fs.StringVar(&o.repoRoot, "repo-root", envKey(getenv, "repo-root"), "path to the repository root (required unless --scip is set)")
	fs.StringVar(&o.repoName, "repo-name", envKey(getenv, "repo-name"), "logical repo name (default: basename of repo-root)")
	fs.StringVar(&o.since, "since", "", "base commit for incremental ingest")
	fs.BoolVar(&o.full, "full", false, "force full re-ingest")
	fs.StringVar(&o.baseURL, "base-url", envKey(getenv, "base-url"), "tatara-memory base URL")
	fs.StringVar(&o.scipPath, "scip", "", "path to a pre-generated SCIP index.scip file (bypasses repo walk)")
	fs.StringVar(&o.scipRepo, "scip-repo", "", "logical repo name for SCIP ingest (required when --scip is set)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if o.scipPath != "" {
		if o.scipRepo == "" {
			return options{}, errMissingSCIPRepo
		}
		return o, nil
	}
	if o.repoRoot == "" {
		return options{}, errMissingRepoRoot
	}
	if o.repoName == "" {
		o.repoName = filepath.Base(strings.TrimRight(o.repoRoot, "/"))
	}
	return o, nil
}

// envKey maps a kebab-case flag name to its UPPER_SNAKE env var and returns its value.
func envKey(getenv func(string) string, key string) string {
	return getenv(strings.ToUpper(strings.ReplaceAll(key, "-", "_")))
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
