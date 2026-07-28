// Package push sends the code graph and semantic chunks to tatara-memory.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/obs"
)

// maxJobWait caps how long PushChunks will poll for a job to reach terminal
// state. The cron Job activeDeadlineSeconds is a hard backstop, but the
// client should fail fast with a clear error rather than spin forever if the
// server is stuck or returns an unrecognised non-terminal status.
const maxJobWait = 30 * time.Minute

// maxRetries is the number of additional attempts for transient errors on any
// HTTP call (total attempts = maxRetries+1). A shedding server (429 or 503) can
// stay busy for several seconds, so the budget is wider than a single blip.
const maxRetries = 4

// retryDelay is the base backoff between retry attempts; it doubles per attempt
// up to maxBackoff. Overridable in tests.
var retryDelay = 200 * time.Millisecond

// maxBackoff caps the exponential backoff; maxRetryAfter caps how long a
// server-supplied Retry-After is honoured, so a bogus header cannot park an
// ingest Job for hours.
const (
	maxBackoff    = 10 * time.Second
	maxRetryAfter = 60 * time.Second
)

// codeGraphPath is the bulk graph endpoint; also the metrics label for it.
const codeGraphPath = "/code-graph:bulk"

// Client posts to a tatara-memory base URL.
type Client struct {
	base         string
	http         *http.Client
	pollInterval time.Duration
	metrics      *obs.Metrics
}

// New constructs a push client.
func New(base string, hc *http.Client, pollInterval time.Duration) *Client {
	return &Client{base: base, http: hc, pollInterval: pollInterval}
}

// WithMetrics attaches the run's metric registry so push-level counters (graph
// batches, retries, shed responses) land in the same push at job end. Metrics
// are optional: an unset registry simply records nothing.
func (c *Client) WithMetrics(m *obs.Metrics) *Client {
	c.metrics = m
	return c
}

// PushGraph posts a GraphPush and returns the aggregated reconciliation summary.
//
// The push is split into bounded batches (see splitGraphPush): tatara-memory
// reconciles each request in one Postgres transaction, so a whole-repo push held
// a connection of the shared pool for minutes and starved every other caller
// (tatara-memory#82). Batches are sent SEQUENTIALLY on purpose - concurrency
// would put back exactly the pool pressure this removes.
//
// Partial-failure semantics: batches commit independently, so a failure at batch
// k leaves batches 1..k-1 committed and the repo's graph partially updated. That
// is surfaced as a loud error (never swallowed) and the remaining batches are
// NOT sent. The caller fails the run, the operator leaves LastIngestedCommit
// unchanged, and the next attempt replays the same --since window: every batch
// is an idempotent upsert reconciled per file, so a replay converges on the full
// graph.
func (c *Client) PushGraph(ctx context.Context, p contract.GraphPush) (contract.PushResult, error) {
	start := time.Now()
	batches := splitGraphPush(p, MaxBatchRows, MaxBatchFiles)
	var agg contract.PushResult
	agg.Repo = p.Repo
	filesDone := 0
	for i, b := range batches {
		bStart := time.Now()
		var res contract.PushResult
		if _, err := c.doWithRetry(ctx, http.MethodPost, codeGraphPath, b, &res, http.StatusOK); err != nil {
			c.countBatch("err", batchRows(b))
			return contract.PushResult{}, fmt.Errorf(
				"code-graph batch %d/%d failed for repo %s (extractor %q): %d batch(es) covering %d/%d files already committed, so the graph is PARTIALLY updated; the run must fail so the same window is retried and converges: %w",
				i+1, len(batches), p.Repo, p.Extractor, i, filesDone, len(p.Files), err)
		}
		c.countBatch("ok", batchRows(b))
		filesDone += len(b.Files)
		agg.Files += res.Files
		agg.EntitiesUpserted += res.EntitiesUpserted
		agg.EdgesUpserted += res.EdgesUpserted
		if len(batches) > 1 {
			slog.Info("PushGraph batch",
				"action", "PushGraph",
				"repo", p.Repo,
				"extractor", p.Extractor,
				"batch", i+1,
				"batches", len(batches),
				"files", len(b.Files),
				"rows", batchRows(b),
				"duration_ms", time.Since(bStart).Milliseconds())
		}
	}
	slog.Info("PushGraph",
		"action", "PushGraph",
		"repo", p.Repo,
		"extractor", p.Extractor,
		"batches", len(batches),
		"entities", len(p.Entities),
		"edges", len(p.Edges),
		"files", len(p.Files),
		"duration_ms", time.Since(start).Milliseconds())
	return agg, nil
}

// countBatch records one /code-graph:bulk batch attempt outcome.
func (c *Client) countBatch(result string, rows int) {
	if c.metrics == nil {
		return
	}
	c.metrics.CodeGraphBatchesTotal.WithLabelValues(result).Inc()
	c.metrics.CodeGraphBatchRows.Observe(float64(rows))
}

// jobTimeoutErr builds a self-describing timeout error carrying the last
// observed job state, so a stuck-job failure is diagnosable whether the
// deadline lands in the poll wait or mid-request on the status read.
func jobTimeoutErr(job contract.IngestJob, cause error) error {
	return fmt.Errorf("ingest job %s did not reach terminal within %s: status=%s done=%d/%d failed=%d: %w",
		job.ID, maxJobWait, job.Status, job.Done, job.Total, job.Failed, cause)
}

// PushChunks posts a reconcile-aware bulk and polls the resulting job to a
// terminal state. repo is the repository identifier sent as the JSON "repo"
// field; the server requires it when reconcileFiles is non-empty. reconcileFiles,
// when non-empty, instructs the server to purge prior memories for each file
// before inserting items. When both reconcileFiles and items are empty there is
// nothing to do.
func (c *Client) PushChunks(ctx context.Context, repo string, reconcileFiles []string, items []contract.IngestItem) error {
	if len(items) == 0 && len(reconcileFiles) == 0 {
		return nil
	}
	start := time.Now()
	var polls int
	var job contract.IngestJob
	body := contract.BulkMemoriesRequest{Repo: repo, ReconcileFiles: reconcileFiles, Items: items}
	// 200 means the server completed the work synchronously (e.g. a reconcile-only
	// bulk with zero embeddable items) and the body carries the terminal job, so
	// there is nothing to poll. 202 means the bulk was accepted for async
	// processing and we poll /ingest-jobs/{id} to a terminal state.
	status, err := c.doWithRetry(ctx, http.MethodPost, "/memories:bulk", body, &job, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return err
	}

	if status == http.StatusAccepted {
		// Bound how long we poll so a stuck job does not block forever.
		pollCtx, cancel := context.WithTimeout(ctx, maxJobWait)
		defer cancel()

		for !job.Terminal() {
			polls++
			select {
			case <-pollCtx.Done():
				return jobTimeoutErr(job, pollCtx.Err())
			case <-time.After(c.pollInterval):
			}
			// Retry transient poll errors: a momentary 502/503 on the status read
			// must not abort an in-flight job. 4xx (job gone) is not retried.
			if _, err := c.doWithRetry(pollCtx, http.MethodGet, "/ingest-jobs/"+job.ID, nil, &job, http.StatusOK); err != nil {
				// A deadline that lands mid-request surfaces here as a context error
				// from the HTTP call rather than via the select above. Report the
				// same self-describing job state so the timeout is diagnosable
				// regardless of where the deadline fell.
				if pollCtx.Err() != nil {
					return jobTimeoutErr(job, pollCtx.Err())
				}
				return err
			}
		}
	} else if !job.Terminal() {
		// 200 is a synchronous completion: the body must carry a terminal job.
		// A non-terminal 200 violates the server contract; fail loudly rather
		// than silently treat unfinished work as done.
		return fmt.Errorf("/memories:bulk returned 200 with non-terminal job %s status=%q", job.ID, job.Status)
	}
	if job.Status != contract.JobSucceeded {
		return fmt.Errorf("ingest job %s ended %s (failed=%d)", job.ID, job.Status, job.Failed)
	}
	slog.Info("PushChunks",
		"action", "PushChunks",
		"repo", repo,
		"job_id", job.ID,
		"items", len(items),
		"reconcile_files", len(reconcileFiles),
		"polls", polls,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// SemanticMisses asks the server which of the supplied files need semantic
// re-extraction (stored content_sha differs or is absent) and returns their paths.
func (c *Client) SemanticMisses(ctx context.Context, req contract.SemanticMissesRequest) ([]string, error) {
	start := time.Now()
	var misses []string
	if _, err := c.doWithRetry(ctx, http.MethodPost, "/code-graph/semantic-misses", req, &misses, http.StatusOK); err != nil {
		return nil, err
	}
	slog.Info("SemanticMisses",
		"action", "SemanticMisses",
		"repo", req.Repo,
		"files_queried", len(req.Files),
		"misses", len(misses),
		"duration_ms", time.Since(start).Milliseconds())
	return misses, nil
}

// HTTP exposes the underlying HTTP client so callers (e.g. the LLM stage) reuse
// the same authenticated transport.
func (c *Client) HTTP() *http.Client { return c.http }

// doWithRetry calls do() and retries on transient errors (5xx, 429, network) up
// to maxRetries additional attempts. The wait is exponential (retryDelay doubled
// per attempt, capped at maxBackoff) UNLESS the server supplied a Retry-After,
// which is honoured verbatim up to maxRetryAfter - that is the back-pressure
// contract tatara-memory's admission control uses when it sheds a bulk push
// (429 + Retry-After), and a client that ignored it would hammer straight back
// into the starved pool. 4xx errors other than 429 are returned immediately. It
// returns the HTTP status code observed on the successful attempt so callers
// that accept more than one status (e.g. 200 vs 202) can branch on it.
func (c *Client) doWithRetry(ctx context.Context, method, path string, in, out any, want ...int) (int, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		status, err := c.do(ctx, method, path, in, out, want...)
		if err == nil {
			return status, nil
		}
		lastErr = err
		te, ok := transientFrom(err)
		if !ok {
			return status, err
		}
		if te.status == http.StatusTooManyRequests || te.status == http.StatusServiceUnavailable {
			c.countShed(path, te.status)
		}
		if attempt == maxRetries {
			break
		}
		wait := backoff(attempt)
		if te.retryAfter > 0 {
			wait = min(te.retryAfter, maxRetryAfter)
		}
		c.countRetry(path, te.reason())
		slog.Warn("push retrying after transient error",
			"action", "push_retry",
			"path", path,
			"status", te.status,
			"attempt", attempt+1,
			"max_attempts", maxRetries+1,
			"retry_after_ms", te.retryAfter.Milliseconds(),
			"wait_ms", wait.Milliseconds(),
			"error", te.Error())
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(wait):
		}
	}
	return 0, lastErr
}

// backoff is the exponential wait before attempt+1, capped at maxBackoff.
func backoff(attempt int) time.Duration {
	d := retryDelay << attempt //nolint:gosec // attempt is bounded by maxRetries
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// countRetry records one retried request.
func (c *Client) countRetry(path, reason string) {
	if c.metrics == nil {
		return
	}
	c.metrics.PushRetriesTotal.WithLabelValues(path, reason).Inc()
}

// countShed records one server load-shed response (429 or 503), whether or not
// it is retried, so client-visible back-pressure is measurable on its own.
func (c *Client) countShed(path string, status int) {
	if c.metrics == nil {
		return
	}
	c.metrics.PushShedResponsesTotal.WithLabelValues(path, strconv.Itoa(status)).Inc()
}

// transientError wraps an error that is eligible for retry. status is the HTTP
// status that produced it (0 for a network error) and retryAfter carries the
// server's Retry-After when it sent one.
type transientError struct {
	cause      error
	status     int
	retryAfter time.Duration
}

func (e *transientError) Error() string { return e.cause.Error() }
func (e *transientError) Unwrap() error { return e.cause }

// reason labels the retry cause for metrics.
func (e *transientError) reason() string {
	switch e.status {
	case 0:
		return "network"
	case http.StatusTooManyRequests:
		return "shed_429"
	case http.StatusServiceUnavailable:
		return "shed_503"
	default:
		return "server_5xx"
	}
}

// transientFrom returns the transient error behind err, if any.
func transientFrom(err error) (*transientError, bool) {
	te, ok := err.(*transientError) //nolint:errorlint
	return te, ok
}

// parseRetryAfter reads an RFC 9110 Retry-After header in either form
// (delay-seconds or HTTP-date). A missing, malformed, or past value returns 0,
// which leaves the caller on its own exponential backoff.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, want ...int) (int, error) {
	var rdr io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("marshal %s: %w", path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("request %s: %w", path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Network/transport errors are transient.
		return 0, &transientError{cause: fmt.Errorf("call %s: %w", path, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if !statusAccepted(resp.StatusCode, want) {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		// Drain the remainder so the connection can be returned to the keep-alive
		// pool - critical on the retry path where 5xx/429 responses are common.
		_, _ = io.Copy(io.Discard, resp.Body)
		err := fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, string(b))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return resp.StatusCode, &transientError{
				cause:      err,
				status:     resp.StatusCode,
				retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			}
		}
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	// Drain any unread bytes so the underlying TCP connection can be reused
	// (HTTP keep-alive). json.Decoder stops at the first JSON value; trailing
	// newlines are common and would otherwise prevent connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// statusAccepted reports whether got is one of the acceptable status codes.
func statusAccepted(got int, want []int) bool {
	for _, w := range want {
		if got == w {
			return true
		}
	}
	return false
}
