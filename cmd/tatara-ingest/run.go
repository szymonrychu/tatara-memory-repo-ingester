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
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/obs"
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
	httpTimeout     time.Duration
	crossRepoPrefix string
	scipPath        string
	scipRepo        string
	metricsPushURL  string
	getenv          func(string) string
	// newRegistry builds the analyzer registry; nil means analyze.Default. A test
	// seam (like getenv) so a hard-erroring analyzer can be injected to exercise
	// the quarantine path deterministically.
	newRegistry func(crossRepoPrefix, repoRoot string) *analyze.Registry
}

func run(ctx context.Context, o options, hc *http.Client) (retErr error) {
	m := obs.New()
	m.IngestRunsTotal.Inc()
	start := time.Now()
	slog.Info("ingest start", "repo", o.repoName, "since", o.since, "full", o.full, "scip", o.scipPath != "")

	// Short-lived Jobs cannot be scraped; push gathered metrics at job end.
	// Best-effort: a push failure is logged and never fails the ingest. Deferred
	// so it fires on every return path (SCIP, normal, and error exits).
	// Also records the terminal result counter and total duration on every path.
	defer func() {
		m.IngestStageDuration.WithLabelValues("total").Observe(time.Since(start).Seconds())
		result := "success"
		if retErr != nil {
			result = "failure"
		}
		m.IngestRunResultTotal.WithLabelValues(result).Inc()
		if o.metricsPushURL != "" {
			// Use a plain client so the memory-audience OIDC bearer (hc) is not
			// sent to the operator's pushmetrics endpoint (different audience).
			plainHC := &http.Client{Timeout: orDur(o.httpTimeout)}
			if err := m.Push(ctx, o.metricsPushURL, plainHC); err != nil {
				slog.Error("metrics push failed", "url", o.metricsPushURL, "error", err) //nolint:gosec // G706: url and err are internal values, not HTTP input
			}
		}
	}()

	if o.scipPath != "" {
		return runSCIP(ctx, o, hc, m)
	}
	changes, err := walk.Diff(o.repoRoot, o.since, o.full)
	if err != nil {
		return err
	}

	// touched is every file in the diff (code-graph Files + memories reconcile_files).
	// analyzeFiles is only A/M/renamed-new (the files we re-analyze and chunk).
	// Use a set to deduplicate: renames append both old and new path, and an edge
	// case could produce duplicates if both appear under separate records.
	seenTouched := make(map[string]struct{})
	var touched, analyzeFiles []string
	addTouched := func(p string) {
		if _, ok := seenTouched[p]; !ok {
			seenTouched[p] = struct{}{}
			touched = append(touched, p)
		}
	}
	for _, ch := range changes.Files {
		addTouched(ch.Path)
		if ch.Status == 'R' && ch.OldPath != "" {
			// For renames the old path is never analyzed, but must appear in the
			// purge sets (touched/Files + reconcile_files) so the server removes
			// stale entities and chunks for the old location.
			addTouched(ch.OldPath)
		}
		switch ch.Status {
		case 'A', 'M', 'R':
			analyzeFiles = append(analyzeFiles, ch.Path)
		}
	}

	newReg := o.newRegistry
	if newReg == nil {
		newReg = analyze.Default
	}
	reg := newReg(o.crossRepoPrefix, o.repoRoot)
	groups := reg.Group(analyzeFiles)

	// failedFiles tracks A/M/R files excluded from the chunk reconcile set so the
	// server does not purge their existing chunks when no replacement was produced.
	// Two sources feed it:
	//   - per-file soft errors (res.FailedFiles): a single unreadable/unparseable
	//     file the analyzer skipped without aborting its batch.
	//   - a quarantined analyzer group (see quarantined below).
	// Both are deterministic and file-local; failing the run on them would wedge
	// ingestion. Deleted files are unaffected: they never go through analyze and
	// must always be reconciled.
	failedFiles := make(map[string]struct{})

	// quarantined tracks files whose analyzer HARD-errored (whole-batch failure,
	// e.g. a go/packages load fault or a recovered tree-sitter panic). The analyzer
	// produced no entities/edges/chunks for them, so they are excluded from BOTH the
	// graph Files set and the chunk reconcile set: reconciling over them would purge
	// their last-good rows with no replacement (the destructive partial #16 guards
	// against). This completes the fail-closed design (#16) into fail-isolated:
	// instead of aborting the whole run on a hard error, only the failing group is
	// held at its last-good version while every healthy group and future commit keeps
	// flowing. The poison files are re-analyzed the next time they (or, for
	// go/packages, any file in their package) re-enter the diff. quarantined is a
	// subset of failedFiles.
	quarantined := make(map[string]struct{})

	var agg analyze.Result
	for _, a := range reg.Analyzers() {
		fs := groups[a.Name()]
		if len(fs) == 0 {
			continue
		}
		aStart := time.Now()
		res, err := a.Analyze(ctx, o.repoRoot, fs)
		aDur := time.Since(aStart)
		if err != nil {
			// Hard error: quarantine this analyzer's whole file group instead of
			// aborting the run. The files keep their last-good graph and chunk rows
			// (excluded from both reconcile sets below) while the rest of the run
			// completes. WARN loudly with the paths so operator#68 alerting and a
			// human can see a file's graph is being held stale rather than updated.
			slog.Warn("analyzer hard-errored; quarantining its file group, last-good graph and chunks preserved",
				"analyzer", a.Name(), "files", len(fs), "paths", fs, "error", err)
			m.AnalyzerParseErrorsTotal.WithLabelValues(a.Name()).Inc()
			m.IngestFilesQuarantinedTotal.WithLabelValues(a.Name()).Add(float64(len(fs)))
			for _, f := range fs {
				quarantined[f] = struct{}{}
				failedFiles[f] = struct{}{}
			}
			continue
		}
		m.AnalyzerEntitiesTotal.WithLabelValues(a.Name()).Add(float64(len(res.Entities)))
		m.AnalyzerEdgesTotal.WithLabelValues(a.Name()).Add(float64(len(res.Edges)))
		m.AnalyzerDuration.WithLabelValues(a.Name()).Observe(aDur.Seconds())
		if res.ParseErrors > 0 {
			m.AnalyzerParseErrorsTotal.WithLabelValues(a.Name()).Add(float64(res.ParseErrors))
		}
		// Per-file read/parse failures: the analyzer skipped these (no replacement
		// chunks produced) but did not abort the batch. They must be excluded from
		// reconcile so the server does not purge their existing chunks.
		for _, f := range res.FailedFiles {
			failedFiles[f] = struct{}{}
		}
		slog.Info("analyzer complete",
			"analyzer", a.Name(),
			"files", len(fs),
			"entities", len(res.Entities),
			"edges", len(res.Edges),
			"duration_ms", aDur.Milliseconds())
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

	// graphFiles is the graph reconcile scope: touched minus quarantined. A
	// quarantined group's analyzer emitted no entities, so leaving its files in
	// Files would reconcile-purge their last-good graph rows with no replacement.
	// Deleted and renamed-old paths are never quarantined (never analyzed) and stay
	// in graphFiles so their stale rows are still purged correctly.
	graphFiles := touched
	if len(quarantined) > 0 {
		graphFiles = make([]string, 0, len(touched))
		for _, f := range touched {
			if _, q := quarantined[f]; !q {
				graphFiles = append(graphFiles, f)
			}
		}
	}

	// When every touched file was quarantined (all A/M/R files in failing groups and
	// no deletions/renames) there is nothing healthy to push and the server rejects
	// an empty Files set. Skip the graph push but still advance the commit so future
	// windows keep flowing.
	if len(graphFiles) > 0 {
		pushStart := time.Now()
		if _, err := cl.PushGraph(ctx, contract.GraphPush{
			Repo: o.repoName, Commit: commit, Extractor: contract.ExtractorAST, Files: graphFiles,
			Entities: agg.Entities, Edges: agg.Edges, Symbols: agg.Symbols,
			Hyperedges: agg.Hyperedges,
		}); err != nil {
			m.PushRequestsTotal.WithLabelValues("/code-graph:bulk", "err").Inc()
			return err
		}
		m.PushRequestsTotal.WithLabelValues("/code-graph:bulk", "ok").Inc()
		m.IngestStageDuration.WithLabelValues("push_graph").Observe(time.Since(pushStart).Seconds())
	}

	// Build reconcile list: exclude files whose analyzer failed (soft per-file errors
	// and quarantined groups, both in failedFiles) so existing chunks are not purged
	// when no replacement was produced. Deleted files (not in failedFiles since they
	// are never analyzed) are always reconciled.
	var reconcile []string
	if !changes.FullSet {
		for _, f := range touched {
			if _, failed := failedFiles[f]; !failed {
				reconcile = append(reconcile, f)
			}
		}
	} // else: first/full ingest is insert-only (reconcile stays nil)
	chunksStart := time.Now()
	if err := cl.PushChunks(ctx, o.repoName, reconcile, push.ItemsFromChunks(o.repoName, agg.Chunks)); err != nil {
		m.PushRequestsTotal.WithLabelValues("/memories:bulk", "err").Inc()
		return err
	}
	m.PushRequestsTotal.WithLabelValues("/memories:bulk", "ok").Inc()
	m.IngestStageDuration.WithLabelValues("push_chunks").Observe(time.Since(chunksStart).Seconds())

	// Best-effort semantic stage: errors are logged and never fail the ingest.
	runSemantic(ctx, o, cl, commit, changes, m)

	slog.Info("ingest complete",
		"repo", o.repoName, "files", len(touched),
		"analyzed", len(analyzeFiles),
		"quarantined", len(quarantined),
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

func runSCIP(ctx context.Context, o options, hc *http.Client, m *obs.Metrics) error {
	start := time.Now()
	gp, err := scip.Parse(o.scipPath, o.scipRepo)
	if err != nil {
		return err
	}
	gp.Extractor = contract.ExtractorSCIP
	m.SCIPEntitiesTotal.Add(float64(len(gp.Entities)))
	m.SCIPEdgesTotal.Add(float64(len(gp.Edges)))
	cl := push.New(o.baseURL, hc, pollOr(o.pollInterval))
	if _, err := cl.PushGraph(ctx, gp); err != nil {
		m.PushRequestsTotal.WithLabelValues("/code-graph:bulk", "err").Inc()
		return err
	}
	m.PushRequestsTotal.WithLabelValues("/code-graph:bulk", "ok").Inc()
	dur := time.Since(start)
	m.IngestStageDuration.WithLabelValues("scip").Observe(dur.Seconds())
	slog.Info("scip ingest complete",
		"repo", o.scipRepo,
		"files", len(gp.Files),
		"entities", len(gp.Entities),
		"edges", len(gp.Edges),
		"duration_ms", dur.Milliseconds())
	return nil
}

func headCommit(repoRoot string) string {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output() //nolint:gosec
	if err != nil {
		slog.Warn("headCommit: git rev-parse failed; ingest will have no commit pin", "repoRoot", repoRoot, "error", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runSemantic is the best-effort LLM extraction stage. It is a no-op when the
// OpenAI key is unset or SEMANTIC_INGEST=false. Any failure (misses call, LLM,
// parse, push) is logged and swallowed so it never fails the AST ingest.
func runSemantic(ctx context.Context, o options, cl *push.Client, commit string, changes walk.Changes, m *obs.Metrics) {
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
		m.PushRequestsTotal.WithLabelValues("/code-graph/semantic-misses", "err").Inc()
		return
	}
	m.PushRequestsTotal.WithLabelValues("/code-graph/semantic-misses", "ok").Inc()
	m.SemanticMissesTotal.Add(float64(len(misses)))
	if len(misses) == 0 {
		return
	}

	// Build a path->contentSHA index once for O(files) lookup below.
	shaByPath := make(map[string]string, len(changes.Files))
	for _, ch := range changes.Files {
		shaByPath[ch.Path] = ch.ContentSHA
	}

	// Load miss-file contents and chunk them.
	repoRootAbs := filepath.Clean(o.repoRoot)
	var loaded []semantic.LoadedFile
	var loadedPaths []string
	fileSHAs := map[string]string{}
	for _, p := range misses {
		clean := filepath.Clean(filepath.Join(repoRootAbs, p))
		if !strings.HasPrefix(clean, repoRootAbs+string(os.PathSeparator)) {
			slog.Warn("semantic: miss path escapes repoRoot; skipping", "file", p, "repoRoot", repoRootAbs)
			continue
		}
		b, err := os.ReadFile(clean) //nolint:gosec
		if err != nil {
			slog.Warn("semantic: unreadable miss file; skipping", "file", p, "error", err)
			continue
		}
		loaded = append(loaded, semantic.LoadedFile{Path: p, Content: string(b)})
		loadedPaths = append(loadedPaths, p)
		fileSHAs[p] = shaByPath[p]
	}
	if len(loaded) == 0 {
		return
	}
	chunks := semantic.Chunk(loaded, semantic.DefaultChunkBudget())

	// Plain HTTP client, NOT cl.HTTP(): the memory client's transport injects the
	// tatara OIDC client-credentials bearer on every request. OpenAI is not
	// OIDC-gated and rejects that JWT with "invalid_issuer", so the LLM call must
	// carry only its own OpenAI Bearer (set by the llm client).
	client := llm.New(llm.ConfigFromEnv(getenv), &http.Client{Timeout: orDur(o.httpTimeout)})
	results := make([]analyze.Result, len(chunks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, ck := range chunks {
		i, ck := i, ck
		g.Go(func() error {
			res, ok := extractChunk(gctx, o.repoName, client, ck, i+1, len(chunks))
			if ok {
				results[i] = res
				m.SemanticChunkExtractionsTotal.WithLabelValues("ok").Inc()
				m.LLMCallsTotal.WithLabelValues("ok").Inc()
			} else {
				m.SemanticChunkExtractionsTotal.WithLabelValues("fail").Inc()
				m.LLMCallsTotal.WithLabelValues("fail").Inc()
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
		Files:      loadedPaths,
		Entities:   aggSem.Entities,
		Edges:      aggSem.Edges,
		Hyperedges: aggSem.Hyperedges,
		FileSHAs:   fileSHAs,
	}); err != nil {
		slog.Warn("semantic graph push failed", "repo", o.repoName, "error", err)
		m.PushRequestsTotal.WithLabelValues("/code-graph:bulk[semantic]", "err").Inc()
		return
	}
	m.PushRequestsTotal.WithLabelValues("/code-graph:bulk[semantic]", "ok").Inc()
	slog.Info("semantic stage complete",
		"repo", o.repoName, "misses", len(misses), "chunks", len(chunks),
		"entities", len(aggSem.Entities), "edges", len(aggSem.Edges), "hyperedges", len(aggSem.Hyperedges))
}

// extractChunk runs one chunk through the LLM and parser. ok is false on any
// failure (logged WARN), so the caller drops that chunk's contribution.
func extractChunk(ctx context.Context, repo string, client *llm.Client, ck semantic.FileChunk, chunkNum, total int) (analyze.Result, bool) {
	var fl strings.Builder
	var fc strings.Builder
	validFiles := make(map[string]struct{}, len(ck.Files))
	for _, f := range ck.Files {
		fl.WriteString("- ")
		fl.WriteString(f.Path)
		fl.WriteString("\n")
		fc.WriteString("### ")
		fc.WriteString(f.Path)
		fc.WriteString("\n```\n")
		fc.WriteString(f.Content)
		fc.WriteString("\n```\n")
		validFiles[f.Path] = struct{}{}
	}
	prompt := semantic.BuildPrompt(semantic.PromptVars{
		FileList:    strings.TrimRight(fl.String(), "\n"),
		FileContent: strings.TrimRight(fc.String(), "\n"),
		ChunkNum:    chunkNum,
		TotalChunks: total,
	})
	out, err := client.Complete(ctx, prompt)
	if err != nil {
		slog.Warn("semantic LLM call failed; skipping chunk", "repo", repo, "chunk", chunkNum, "error", err) //nolint:gosec // G706: repo and err are internal values, not HTTP input
		return analyze.Result{}, false
	}
	res, err := semantic.ParseFragment(repo, []byte(out), validFiles)
	if err != nil {
		slog.Warn("semantic parse failed; skipping chunk", "repo", repo, "chunk", chunkNum, "error", err) //nolint:gosec // G706: repo and err are internal values, not HTTP input
		return analyze.Result{}, false
	}
	return res, true
}
