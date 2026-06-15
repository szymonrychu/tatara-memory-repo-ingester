package push_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

func TestPushGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/code-graph:bulk", r.URL.Path)
		var p contract.GraphPush
		require.NoError(t, json.NewDecoder(r.Body).Decode(&p))
		require.Equal(t, "tatara-cli", p.Repo)
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(contract.PushResult{Repo: p.Repo, EntitiesUpserted: len(p.Entities)})
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	res, err := c.PushGraph(context.Background(), contract.GraphPush{Repo: "tatara-cli", Entities: []contract.Entity{{ID: "x"}}})
	require.NoError(t, err)
	require.Equal(t, 1, res.EntitiesUpserted)
}

func TestPushChunksPollsToTerminal(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/memories:bulk":
			var req contract.BulkMemoriesRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Len(t, req.Items, 1)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: "running"})
		case strings.HasPrefix(r.URL.Path, "/ingest-jobs/"):
			polls++
			st := "running"
			if polls >= 2 {
				st = contract.JobSucceeded
			}
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: st, Total: 1, Done: 1})
		}
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	err := c.PushChunks(context.Background(), "r", nil, []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
	require.NoError(t, err)
	require.GreaterOrEqual(t, polls, 2)
}

func TestPushChunksPartialIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/memories:bulk" {
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: "running"})
			return
		}
		_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobPartial, Failed: 1})
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	err := c.PushChunks(context.Background(), "r", nil, []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
	require.Error(t, err)
}

func TestPushChunksSendsReconcileFiles(t *testing.T) {
	var gotReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/memories:bulk":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded, Total: 1, Done: 1})
		case strings.HasPrefix(r.URL.Path, "/ingest-jobs/"):
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded, Total: 1, Done: 1})
		}
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	err := c.PushChunks(context.Background(), "r",
		[]string{"a.go", "gone.go"},
		[]contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a.go", "gone.go"}, gotReq.ReconcileFiles)
	require.Len(t, gotReq.Items, 1)
}

func TestPushChunksReconcileOnlyDeletion(t *testing.T) {
	// A pure deletion: reconcile_files set, no items. Must still POST and reconcile.
	var gotReq contract.BulkMemoriesRequest
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/memories:bulk":
			posted = true
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded})
		case strings.HasPrefix(r.URL.Path, "/ingest-jobs/"):
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded})
		}
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	err := c.PushChunks(context.Background(), "r", []string{"gone.go"}, nil)
	require.NoError(t, err)
	require.True(t, posted, "deletion-only reconcile must still POST /memories:bulk")
	require.Equal(t, []string{"gone.go"}, gotReq.ReconcileFiles)
	require.Empty(t, gotReq.Items)
}

func TestPushChunksSendsRepoWhenReconciling(t *testing.T) {
	// RED: PushChunks must include "repo" in the JSON body when reconcile_files is
	// non-empty. Without the Repo field the memory API returns 400
	// {"error":"repo is required when reconcile_files is set"}.
	cases := []struct {
		name           string
		repo           string
		reconcileFiles []string
		items          []contract.IngestItem
		wantRepo       string
	}{
		{
			name:           "reconcile with items",
			repo:           "tatara-cli",
			reconcileFiles: []string{"a.go", "gone.go"},
			items:          []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}},
			wantRepo:       "tatara-cli",
		},
		{
			name:           "reconcile only deletion",
			repo:           "tatara-memory",
			reconcileFiles: []string{"gone.go"},
			items:          nil,
			wantRepo:       "tatara-memory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotReq contract.BulkMemoriesRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/memories:bulk":
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
					w.WriteHeader(202)
					_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded})
				case strings.HasPrefix(r.URL.Path, "/ingest-jobs/"):
					_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded})
				}
			}))
			defer srv.Close()
			c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
			err := c.PushChunks(context.Background(), tc.repo, tc.reconcileFiles, tc.items)
			require.NoError(t, err)
			require.Equal(t, tc.wantRepo, gotReq.Repo, "repo must be set in /memories:bulk body")
			require.ElementsMatch(t, tc.reconcileFiles, gotReq.ReconcileFiles)
		})
	}
}

func TestPushChunksNoopWhenNothingToDo(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "j1", Status: contract.JobSucceeded})
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	require.NoError(t, c.PushChunks(context.Background(), "", nil, nil))
	require.False(t, posted, "no reconcile and no items must not POST")
}

func TestSemanticMissesReturnsMissPaths(t *testing.T) {
	var gotReq contract.SemanticMissesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/code-graph/semantic-misses", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode([]string{"a.go", "c.go"})
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	misses, err := c.SemanticMisses(context.Background(), contract.SemanticMissesRequest{
		Repo: "r",
		Files: []contract.FileSHA{
			{Path: "a.go", ContentSHA: "s1"},
			{Path: "b.go", ContentSHA: "s2"},
			{Path: "c.go", ContentSHA: "s3"},
		},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a.go", "c.go"}, misses)
	require.Equal(t, "r", gotReq.Repo)
	require.Len(t, gotReq.Files, 3)
}

func TestSemanticMissesPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.SemanticMisses(context.Background(), contract.SemanticMissesRequest{Repo: "r"})
	require.Error(t, err)
}

func TestClientHTTPAccessor(t *testing.T) {
	hc := &http.Client{}
	c := push.New("http://x", hc, time.Millisecond)
	require.Same(t, hc, c.HTTP())
}

// TestPushGraphLogsInfoOnSuccess verifies that a successful PushGraph call
// emits an INFO log with action, repo, duration_ms (hard rule 12).
func TestPushGraphLogsInfoOnSuccess(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(contract.PushResult{Repo: "myrepo", EntitiesUpserted: 1})
	}))
	defer srv.Close()

	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	_, err := c.PushGraph(context.Background(), contract.GraphPush{Repo: "myrepo", Entities: []contract.Entity{{ID: "x"}}})
	require.NoError(t, err)

	logs := logBuf.String()
	assert.Contains(t, logs, "PushGraph", "INFO log must mention the action")
	assert.Contains(t, logs, "myrepo", "INFO log must mention the repo")
	assert.Contains(t, logs, "duration_ms", "INFO log must include duration_ms")
}

// TestPushChunksLogsInfoOnSuccess verifies that a successful PushChunks call
// emits an INFO log with action, repo, job_id, and duration_ms (hard rule 12).
func TestPushChunksLogsInfoOnSuccess(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/memories:bulk":
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "job42", Status: contract.JobSucceeded, Total: 1, Done: 1})
		default:
			_ = json.NewEncoder(w).Encode(contract.IngestJob{ID: "job42", Status: contract.JobSucceeded, Total: 1, Done: 1})
		}
	}))
	defer srv.Close()

	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	err := c.PushChunks(context.Background(), "myrepo", nil, []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
	require.NoError(t, err)

	logs := logBuf.String()
	assert.Contains(t, logs, "PushChunks", "INFO log must mention the action")
	assert.Contains(t, logs, "myrepo", "INFO log must mention the repo")
	assert.Contains(t, logs, "duration_ms", "INFO log must include duration_ms")
	assert.Contains(t, logs, "job42", "INFO log must include job_id")
}

// TestSemanticMissesLogsInfoOnSuccess verifies that SemanticMisses emits an
// INFO log with action, repo, count, duration_ms (hard rule 12).
func TestSemanticMissesLogsInfoOnSuccess(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode([]string{"a.go"})
	}))
	defer srv.Close()

	c := push.New(srv.URL, http.DefaultClient, time.Millisecond)
	misses, err := c.SemanticMisses(context.Background(), contract.SemanticMissesRequest{Repo: "myrepo"})
	require.NoError(t, err)
	require.Len(t, misses, 1)

	logs := logBuf.String()
	assert.Contains(t, logs, "SemanticMisses", "INFO log must mention the action")
	assert.Contains(t, logs, "myrepo", "INFO log must mention the repo")
	assert.Contains(t, logs, "duration_ms", "INFO log must include duration_ms")
}
