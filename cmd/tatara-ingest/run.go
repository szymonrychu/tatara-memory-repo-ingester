package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/scip"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/walk"
)

var (
	errMissingRepoRoot = errors.New("--repo-root is required")
	errMissingSCIPRepo = errors.New("--scip-repo is required when --scip is set")
)

type options struct {
	repoRoot        string
	repoName        string
	since           string
	full            bool
	baseURL         string
	pollInterval    time.Duration
	crossRepoPrefix string
	scipPath        string
	scipRepo        string
}

func run(ctx context.Context, o options, hc *http.Client) error {
	if o.scipPath != "" {
		return runSCIP(ctx, o, hc)
	}
	start := time.Now()
	changes, err := walk.Diff(o.repoRoot, o.since, o.full)
	if err != nil {
		return err
	}

	// touched is every file in the diff (code-graph Files + memories reconcile_files).
	// analyzeFiles is only A/M/renamed-new (the files we re-analyze and chunk).
	var touched, analyzeFiles []string
	for _, ch := range changes.Files {
		touched = append(touched, ch.Path)
		if ch.Status == 'R' && ch.OldPath != "" {
			// For renames the old path is never analyzed, but must appear in the
			// purge sets (touched/Files + reconcile_files) so the server removes
			// stale entities and chunks for the old location.
			touched = append(touched, ch.OldPath)
		}
		switch ch.Status {
		case 'A', 'M', 'R':
			analyzeFiles = append(analyzeFiles, ch.Path)
		}
	}

	reg := analyze.Default(o.crossRepoPrefix)
	groups := reg.Group(analyzeFiles)

	var agg analyze.Result
	for _, a := range reg.Analyzers() {
		fs := groups[a.Name()]
		if len(fs) == 0 {
			continue
		}
		res, err := a.Analyze(ctx, o.repoRoot, fs)
		if err != nil {
			slog.Warn("analyzer failed", "analyzer", a.Name(), "error", err)
			continue
		}
		agg.Entities = append(agg.Entities, res.Entities...)
		agg.Edges = append(agg.Edges, res.Edges...)
		agg.Chunks = append(agg.Chunks, res.Chunks...)
		agg.Symbols = append(agg.Symbols, res.Symbols...)
		agg.Hyperedges = append(agg.Hyperedges, res.Hyperedges...)
	}

	commit := headCommit(o.repoRoot)
	cl := push.New(o.baseURL, hc, pollOr(o.pollInterval))
	if _, err := cl.PushGraph(ctx, contract.GraphPush{
		Repo: o.repoName, Commit: commit, Files: touched,
		Entities: agg.Entities, Edges: agg.Edges, Symbols: agg.Symbols,
		Hyperedges: agg.Hyperedges,
	}); err != nil {
		return err
	}
	reconcile := touched
	if changes.FullSet {
		reconcile = nil // first/full ingest is insert-only (no reconcile)
	}
	if err := cl.PushChunks(ctx, reconcile, push.ItemsFromChunks(o.repoName, agg.Chunks)); err != nil {
		return err
	}
	slog.Info("ingest complete",
		"repo", o.repoName, "files", len(touched),
		"analyzed", len(analyzeFiles),
		"entities", len(agg.Entities), "edges", len(agg.Edges),
		"chunks", len(agg.Chunks), "symbols", len(agg.Symbols),
		"full", changes.FullSet,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func pollOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 2 * time.Second
	}
	return d
}

func runSCIP(ctx context.Context, o options, hc *http.Client) error {
	start := time.Now()
	gp, err := scip.Parse(o.scipPath, o.scipRepo)
	if err != nil {
		return err
	}
	cl := push.New(o.baseURL, hc, pollOr(o.pollInterval))
	if _, err := cl.PushGraph(ctx, gp); err != nil {
		return err
	}
	slog.Info("scip ingest complete",
		"repo", o.scipRepo,
		"files", len(gp.Files),
		"entities", len(gp.Entities),
		"edges", len(gp.Edges),
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func headCommit(repoRoot string) string {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
