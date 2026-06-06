package push_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
			var req struct {
				Items []contract.IngestItem `json:"items"`
			}
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
	err := c.PushChunks(context.Background(), []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
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
	err := c.PushChunks(context.Background(), []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}})
	require.Error(t, err)
}
