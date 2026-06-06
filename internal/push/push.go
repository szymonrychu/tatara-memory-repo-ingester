// Package push sends the code graph and semantic chunks to tatara-memory.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

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
	var res contract.PushResult
	if err := c.do(ctx, http.MethodPost, "/code-graph:bulk", p, http.StatusOK, &res); err != nil {
		return contract.PushResult{}, err
	}
	return res, nil
}

// PushChunks posts chunks and polls the resulting job to a terminal state.
func (c *Client) PushChunks(ctx context.Context, items []contract.IngestItem) error {
	if len(items) == 0 {
		return nil
	}
	var job contract.IngestJob
	body := struct {
		Items []contract.IngestItem `json:"items"`
	}{Items: items}
	if err := c.do(ctx, http.MethodPost, "/memories:bulk", body, http.StatusAccepted, &job); err != nil {
		return err
	}
	for !job.Terminal() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollInterval):
		}
		if err := c.do(ctx, http.MethodGet, "/ingest-jobs/"+job.ID, nil, http.StatusOK, &job); err != nil {
			return err
		}
	}
	if job.Status != contract.JobSucceeded {
		return fmt.Errorf("ingest job %s ended %s (failed=%d)", job.ID, job.Status, job.Failed)
	}
	return nil
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
		return fmt.Errorf("call %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, string(b))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}
