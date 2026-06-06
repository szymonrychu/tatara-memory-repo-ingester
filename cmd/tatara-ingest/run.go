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
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/walk"
)

var errMissingRepoRoot = errors.New("--repo-root is required")

type options struct {
	repoRoot     string
	repoName     string
	since        string
	full         bool
	baseURL      string
	pollInterval time.Duration
}

func run(ctx context.Context, o options, hc *http.Client) error {
	start := time.Now()
	files, err := walk.Changed(o.repoRoot, o.since, o.full)
	if err != nil {
		return err
	}
	reg := analyze.Default()
	groups := reg.Group(files)

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
	}

	commit := headCommit(o.repoRoot)
	cl := push.New(o.baseURL, hc, pollOr(o.pollInterval))
	if _, err := cl.PushGraph(ctx, contract.GraphPush{
		Repo: o.repoName, Commit: commit, Files: files,
		Entities: agg.Entities, Edges: agg.Edges,
	}); err != nil {
		return err
	}
	if err := cl.PushChunks(ctx, push.ItemsFromChunks(o.repoName, agg.Chunks)); err != nil {
		return err
	}
	slog.Info("ingest complete",
		"repo", o.repoName, "files", len(files),
		"entities", len(agg.Entities), "edges", len(agg.Edges),
		"chunks", len(agg.Chunks), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func pollOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 2 * time.Second
	}
	return d
}

func headCommit(repoRoot string) string {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
