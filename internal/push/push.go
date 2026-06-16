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
	"time"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// maxJobWait caps how long PushChunks will poll for a job to reach terminal
// state. The cron Job activeDeadlineSeconds is a hard backstop, but the
// client should fail fast with a clear error rather than spin forever if the
// server is stuck or returns an unrecognised non-terminal status.
const maxJobWait = 30 * time.Minute

// maxRetries is the number of additional attempts for transient errors on any
// HTTP call (total attempts = maxRetries+1).
const maxRetries = 2

// retryDelay is the base backoff between retry attempts; overridable in tests.
var retryDelay = 200 * time.Millisecond

// Client posts to a tatara-memory base URL.
type Client struct {
	base         string
	http         *http.Client
	pollInterval time.Duration
}

// New constructs a push client.
func New(base string, hc *http.Client, pollInterval time.Duration) *Client {
	return &Client{base: base, http: hc, pollInterval: pollInterval}
}

// PushGraph posts a GraphPush synchronously and returns the reconciliation summary.
func (c *Client) PushGraph(ctx context.Context, p contract.GraphPush) (contract.PushResult, error) {
	start := time.Now()
	var res contract.PushResult
	if err := c.doWithRetry(ctx, http.MethodPost, "/code-graph:bulk", p, http.StatusOK, &res); err != nil {
		return contract.PushResult{}, err
	}
	slog.Info("PushGraph",
		"action", "PushGraph",
		"repo", p.Repo,
		"entities", len(p.Entities),
		"edges", len(p.Edges),
		"files", len(p.Files),
		"duration_ms", time.Since(start).Milliseconds())
	return res, nil
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
	if err := c.doWithRetry(ctx, http.MethodPost, "/memories:bulk", body, http.StatusAccepted, &job); err != nil {
		return err
	}

	// Bound how long we poll so a stuck job does not block forever.
	pollCtx, cancel := context.WithTimeout(ctx, maxJobWait)
	defer cancel()

	for !job.Terminal() {
		polls++
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("ingest job %s did not reach terminal within %s: status=%s done=%d/%d failed=%d: %w",
				job.ID, maxJobWait, job.Status, job.Done, job.Total, job.Failed, pollCtx.Err())
		case <-time.After(c.pollInterval):
		}
		// Retry transient poll errors: a momentary 502/503 on the status read
		// must not abort an in-flight job. 4xx (job gone) is not retried.
		if err := c.doWithRetry(pollCtx, http.MethodGet, "/ingest-jobs/"+job.ID, nil, http.StatusOK, &job); err != nil {
			return err
		}
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
	if err := c.doWithRetry(ctx, http.MethodPost, "/code-graph/semantic-misses", req, http.StatusOK, &misses); err != nil {
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

// doWithRetry calls do() and retries on transient errors (5xx, 429, network)
// up to maxRetries additional attempts with a fixed backoff. 4xx errors are
// returned immediately.
func (c *Client) doWithRetry(ctx context.Context, method, path string, in any, want int, out any) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		err := c.do(ctx, method, path, in, want, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransient(err) {
			return err
		}
	}
	return lastErr
}

// transientError wraps an error that is eligible for retry.
type transientError struct{ cause error }

func (e *transientError) Error() string { return e.cause.Error() }
func (e *transientError) Unwrap() error { return e.cause }

// isTransient returns true when err was produced by a retryable condition.
func isTransient(err error) bool {
	_, ok := err.(*transientError) //nolint:errorlint
	return ok
}

func (c *Client) do(ctx context.Context, method, path string, in any, want int, out any) error {
	var rdr io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Network/transport errors are transient.
		return &transientError{cause: fmt.Errorf("call %s: %w", path, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		// Drain the remainder so the connection can be returned to the keep-alive
		// pool - critical on the retry path where 5xx/429 responses are common.
		_, _ = io.Copy(io.Discard, resp.Body)
		err := fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, string(b))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return &transientError{cause: err}
		}
		return err
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	// Drain any unread bytes so the underlying TCP connection can be reused
	// (HTTP keep-alive). json.Decoder stops at the first JSON value; trailing
	// newlines are common and would otherwise prevent connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
