package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/llm"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/scip"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/semantic"
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
	getenv          func(string) string
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

	if len(touched) == 0 {
		commit := headCommit(o.repoRoot)
		slog.Info("ingest no-op: no changed files", "repo", o.repoName, "commit", commit)
		return nil
	}

	commit := headCommit(o.repoRoot)
	cl := push.New(o.baseURL, hc, pollOr(o.pollInterval))
	if _, err := cl.PushGraph(ctx, contract.GraphPush{
		Repo: o.repoName, Commit: commit, Extractor: contract.ExtractorAST, Files: touched,
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

	// Best-effort semantic stage: errors are logged and never fail the ingest.
	runSemantic(ctx, o, cl, commit, changes)

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

// shaFor returns the content_sha for an A/M/R analyzed change in the diff.
func shaFor(changes walk.Changes, path string) string {
	for _, ch := range changes.Files {
		if ch.Path == path {
			return ch.ContentSHA
		}
	}
	return ""
}

// runSemantic is the best-effort LLM extraction stage. It is a no-op when the
// OpenAI key is unset or SEMANTIC_INGEST=false. Any failure (misses call, LLM,
// parse, push) is logged and swallowed so it never fails the AST ingest.
func runSemantic(ctx context.Context, o options, cl *push.Client, commit string, changes walk.Changes) {
	getenv := o.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("OPENAI_API_KEY") == "" {
		return
	}
	if strings.EqualFold(getenv("SEMANTIC_INGEST"), "false") {
		return
	}

	// Candidate files: analyzed (A/M/R) changes that have a content_sha.
	var req contract.SemanticMissesRequest
	req.Repo = o.repoName
	for _, ch := range changes.Files {
		switch ch.Status {
		case 'A', 'M', 'R':
			if ch.ContentSHA != "" {
				req.Files = append(req.Files, contract.FileSHA{Path: ch.Path, ContentSHA: ch.ContentSHA})
			}
		}
	}
	if len(req.Files) == 0 {
		return
	}

	misses, err := cl.SemanticMisses(ctx, req)
	if err != nil {
		slog.Warn("semantic-misses failed; skipping semantic stage", "repo", o.repoName, "error", err)
		return
	}
	if len(misses) == 0 {
		return
	}

	// Load miss-file contents and chunk them.
	var loaded []semantic.LoadedFile
	fileSHAs := map[string]string{}
	for _, p := range misses {
		b, err := os.ReadFile(filepath.Join(o.repoRoot, p)) //nolint:gosec
		if err != nil {
			slog.Warn("semantic: unreadable miss file; skipping", "file", p, "error", err)
			continue
		}
		loaded = append(loaded, semantic.LoadedFile{Path: p, Content: string(b)})
		fileSHAs[p] = shaFor(changes, p)
	}
	if len(loaded) == 0 {
		return
	}
	chunks := semantic.Chunk(loaded, semantic.DefaultChunkBudget())

	// Plain HTTP client, NOT cl.HTTP(): the memory client's transport injects the
	// tatara OIDC client-credentials bearer on every request. OpenAI is not
	// OIDC-gated and rejects that JWT with "invalid_issuer", so the LLM call must
	// carry only its own OpenAI Bearer (set by the llm client).
	client := llm.New(llm.ConfigFromEnv(getenv), &http.Client{Timeout: 60 * time.Second})
	results := make([]analyze.Result, len(chunks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, ck := range chunks {
		i, ck := i, ck
		g.Go(func() error {
			res, ok := extractChunk(gctx, o.repoName, client, ck, i+1, len(chunks))
			if ok {
				results[i] = res
			}
			return nil // best-effort: never propagate a chunk error
		})
	}
	_ = g.Wait()

	var aggSem analyze.Result
	for _, r := range results {
		aggSem.Entities = append(aggSem.Entities, r.Entities...)
		aggSem.Edges = append(aggSem.Edges, r.Edges...)
		aggSem.Hyperedges = append(aggSem.Hyperedges, r.Hyperedges...)
	}
	if len(aggSem.Entities) == 0 && len(aggSem.Edges) == 0 && len(aggSem.Hyperedges) == 0 {
		return
	}

	if _, err := cl.PushGraph(ctx, contract.GraphPush{
		Repo: o.repoName, Commit: commit, Extractor: contract.ExtractorSemantic,
		Files:      misses,
		Entities:   aggSem.Entities,
		Edges:      aggSem.Edges,
		Hyperedges: aggSem.Hyperedges,
		FileSHAs:   fileSHAs,
	}); err != nil {
		slog.Warn("semantic graph push failed", "repo", o.repoName, "error", err)
		return
	}
	slog.Info("semantic stage complete",
		"repo", o.repoName, "misses", len(misses), "chunks", len(chunks),
		"entities", len(aggSem.Entities), "edges", len(aggSem.Edges), "hyperedges", len(aggSem.Hyperedges))
}

// extractChunk runs one chunk through the LLM and parser. ok is false on any
// failure (logged WARN), so the caller drops that chunk's contribution.
func extractChunk(ctx context.Context, repo string, client *llm.Client, ck semantic.FileChunk, chunkNum, total int) (analyze.Result, bool) {
	var fl strings.Builder
	for _, f := range ck.Files {
		fl.WriteString("- ")
		fl.WriteString(f.Path)
		fl.WriteString("\n")
	}
	prompt := semantic.BuildPrompt(semantic.PromptVars{
		FileList:    strings.TrimRight(fl.String(), "\n"),
		ChunkNum:    chunkNum,
		TotalChunks: total,
	})
	out, err := client.Complete(ctx, prompt)
	if err != nil {
		slog.Warn("semantic LLM call failed; skipping chunk", "repo", repo, "chunk", chunkNum, "error", err) //nolint:gosec // G706: repo and err are internal values, not HTTP input
		return analyze.Result{}, false
	}
	res, err := semantic.ParseFragment(repo, []byte(out))
	if err != nil {
		slog.Warn("semantic parse failed; skipping chunk", "repo", repo, "chunk", chunkNum, "error", err) //nolint:gosec // G706: repo and err are internal values, not HTTP input
		return analyze.Result{}, false
	}
	return res, true
}
