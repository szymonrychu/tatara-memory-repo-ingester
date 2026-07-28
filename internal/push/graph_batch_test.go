package push_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/obs"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

// bigGraph builds a push whose row count is well past the batch cap.
func bigGraph(nFiles, perFile int) contract.GraphPush {
	p := contract.GraphPush{Repo: "big-repo", Commit: "abc123", Extractor: contract.ExtractorAST}
	for i := 0; i < nFiles; i++ {
		f := fmt.Sprintf("pkg/f%04d.go", i)
		p.Files = append(p.Files, f)
		for j := 0; j < perFile; j++ {
			p.Entities = append(p.Entities, contract.Entity{ID: fmt.Sprintf("%s#e%d", f, j), FilePath: f})
			p.Edges = append(p.Edges, contract.Edge{From: fmt.Sprintf("%s#e%d", f, j), To: "t", Relation: contract.RelCalls, SrcFile: f})
		}
	}
	return p
}

// graphSrv records every /code-graph:bulk body it receives.
func graphSrv(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int) bool) (*httptest.Server, *[]contract.GraphPush) {
	t.Helper()
	var mu sync.Mutex
	var got []contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/code-graph:bulk", r.URL.Path)
		var p contract.GraphPush
		require.NoError(t, json.NewDecoder(r.Body).Decode(&p))
		mu.Lock()
		got = append(got, p)
		n := len(got)
		mu.Unlock()
		if h != nil && !h(w, r, n) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(contract.PushResult{
			Repo: p.Repo, Files: len(p.Files),
			EntitiesUpserted: len(p.Entities), EdgesUpserted: len(p.Edges),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// TestPushGraphSplitsIntoBoundedBatches is the core regression for issue #31:
// a whole-repo graph must not be one long server transaction.
func TestPushGraphSplitsIntoBoundedBatches(t *testing.T) {
	p := bigGraph(push.MaxBatchFiles*3, 4) // 3x the file cap, 8 rows per file
	srv, got := graphSrv(t, nil)

	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	res, err := c.PushGraph(context.Background(), p)
	require.NoError(t, err)

	require.Greater(t, len(*got), 1, "a graph past the cap must be sent as several requests")
	seen := map[string]struct{}{}
	for i, b := range *got {
		assert.LessOrEqual(t, len(b.Files), push.MaxBatchFiles, "batch %d over the file cap", i)
		assert.LessOrEqual(t, len(b.Entities)+len(b.Edges)+len(b.Symbols)+len(b.Hyperedges), push.MaxBatchRows,
			"batch %d over the row cap", i)
		assert.Equal(t, p.Repo, b.Repo)
		assert.Equal(t, p.Commit, b.Commit)
		assert.Equal(t, p.Extractor, b.Extractor)
		for _, f := range b.Files {
			_, dup := seen[f]
			assert.False(t, dup, "file %s sent in two batches", f)
			seen[f] = struct{}{}
		}
	}
	assert.Len(t, seen, len(p.Files), "every file must be pushed")
	assert.Equal(t, len(p.Entities), res.EntitiesUpserted, "result must aggregate every batch")
	assert.Equal(t, len(p.Edges), res.EdgesUpserted)
	assert.Equal(t, len(p.Files), res.Files)
}

// TestPushGraphSmallGraphIsOneRequest pins premortem item 2: batching must not
// multiply request count for the common small incremental push.
func TestPushGraphSmallGraphIsOneRequest(t *testing.T) {
	p := bigGraph(20, 2)
	srv, got := graphSrv(t, nil)
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.PushGraph(context.Background(), p)
	require.NoError(t, err)
	require.Len(t, *got, 1, "a graph under the cap must stay a single request")
}

// TestPushGraphHonoursRetryAfterOn429 covers the shed-load contract the memory
// server is moving to (429 + Retry-After) as well as the current 503.
func TestPushGraphHonoursRetryAfterOn429(t *testing.T) {
	start := time.Now()
	srv, got := graphSrv(t, func(w http.ResponseWriter, _ *http.Request, n int) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("shedding load"))
			return false
		}
		return true
	})
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.PushGraph(context.Background(), bigGraph(2, 1))
	require.NoError(t, err, "a 429 must be retried, not fail the ingest")
	require.Len(t, *got, 2)
	assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond,
		"Retry-After must be honoured, not overridden by the shorter default backoff")
}

// TestPushGraphBacksOffOn503 keeps the client working against a server that
// still sheds with 503 and no Retry-After.
func TestPushGraphBacksOffOn503(t *testing.T) {
	start := time.Now()
	srv, got := graphSrv(t, func(w http.ResponseWriter, _ *http.Request, n int) bool {
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("pool exhausted"))
			return false
		}
		return true
	})
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.PushGraph(context.Background(), bigGraph(2, 1))
	require.NoError(t, err)
	require.Len(t, *got, 3)
	assert.GreaterOrEqual(t, time.Since(start), 600*time.Millisecond,
		"backoff must grow between attempts (200ms + 400ms), not stay flat")
}

// TestPushGraphMidRunFailureIsError asserts partial-failure semantics: a failed
// batch stops the push and surfaces a loud error naming what was already
// committed, so the run fails and the operator retries the same window.
func TestPushGraphMidRunFailureIsError(t *testing.T) {
	p := bigGraph(push.MaxBatchFiles*3, 4)
	srv, got := graphSrv(t, func(w http.ResponseWriter, _ *http.Request, n int) bool {
		if n == 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad batch"))
			return false
		}
		return true
	})
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.PushGraph(context.Background(), p)
	require.Error(t, err, "a failed batch must never be swallowed")
	msg := err.Error()
	assert.Contains(t, msg, "batch 2/", "error must name which batch failed")
	assert.Contains(t, msg, "big-repo", "error must name the repo")
	assert.Contains(t, strings.ToLower(msg), "partial", "error must state the graph is partially updated")
	assert.Len(t, *got, 2, "no further batches may be sent after a batch fails")
}

// TestPushGraphMetrics asserts the observability contract for the new path:
// batches sent, retries, and shed responses are all counted.
func TestPushGraphMetrics(t *testing.T) {
	srv, _ := graphSrv(t, func(w http.ResponseWriter, _ *http.Request, n int) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return false
		}
		return true
	})
	m := obs.New()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond).WithMetrics(m)
	_, err := c.PushGraph(context.Background(), bigGraph(push.MaxBatchFiles*2, 4))
	require.NoError(t, err)

	var body string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	require.NoError(t, m.Push(context.Background(), sink.URL, http.DefaultClient))

	assert.Contains(t, body, "code_graph_batches_total", "batches sent must be counted")
	assert.Contains(t, body, "push_retries_total", "retries must be counted")
	assert.Contains(t, body, "push_shed_responses_total", "shed responses must be counted")
	assert.Contains(t, body, `status="429"`, "the shed status must be labelled")
}
