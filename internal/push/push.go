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
	if _, err := c.doWithRetry(ctx, http.MethodPost, "/code-graph:bulk", p, &res, http.StatusOK); err != nil {
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

// doWithRetry calls do() and retries on transient errors (5xx, 429, network)
// up to maxRetries additional attempts with a fixed backoff. 4xx errors are
// returned immediately. It returns the HTTP status code observed on the
// successful attempt so callers that accept more than one status (e.g. 200 vs
// 202) can branch on which one the server returned.
func (c *Client) doWithRetry(ctx context.Context, method, path string, in, out any, want ...int) (int, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		status, err := c.do(ctx, method, path, in, out, want...)
		if err == nil {
			return status, nil
		}
		lastErr = err
		if !isTransient(err) {
			return status, err
		}
	}
	return 0, lastErr
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
			return resp.StatusCode, &transientError{cause: err}
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
