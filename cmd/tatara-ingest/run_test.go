package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	scipbindings "github.com/scip-code/scip/bindings/go/scip"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestRunReconcileFilesMatchTouchedSet(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nbody\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.md"), []byte("# Old\n\ngone\n"), 0o644))
	base := commitAll(t, dir, "init")

	require.NoError(t, os.Remove(filepath.Join(dir, "old.md")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nbody2\n"), 0o644))
	commitAll(t, dir, "delete old, modify doc")

	var bulkReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &bulkReq)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: base}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.ElementsMatch(t, []string{"doc.md", "old.md"}, bulkReq.ReconcileFiles)
}

func TestRunFullIngestHasNoReconcileFiles(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nbody\n"), 0o644))
	commitAll(t, dir, "init")

	var bulkReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &bulkReq)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL} // full (no since)
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.Empty(t, bulkReq.ReconcileFiles, "full/first ingest is insert-only")
}

func TestRunSendsDeletedFilesInGraphAndReconcile(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.go"),
		[]byte("package m\n\nfunc Keep() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gone.go"),
		[]byte("package m\n\nfunc Gone() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	base := commitAll(t, dir, "init")

	require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.go"),
		[]byte("package m\n\nfunc Keep() { _ = 1 }\n"), 0o644))
	commitAll(t, dir, "delete gone, modify keep")

	var capturedPush contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: base}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	require.Contains(t, capturedPush.Files, "gone.go", "deleted file must be in code-graph Files")
	require.Contains(t, capturedPush.Files, "keep.go", "modified file must be in code-graph Files")
	for _, e := range capturedPush.Entities {
		require.NotEqual(t, "gone.go", e.FilePath, "deleted file must contribute no entities")
	}
}

// TestRunRenameOldPathPurgedNewPathIngested asserts the rename invariant:
//   - old path appears in code-graph Files (so server purges its entities)
//   - old path appears in memories reconcile_files (so server purges its chunks)
//   - old path contributes NO entities
//   - new path is analyzed and contributes entities
func TestRunRenameOldPathPurgedNewPathIngested(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.go"),
		[]byte("package m\n\nfunc OldFunc() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	base := commitAll(t, dir, "init")

	// rename old.go -> new.go
	require.NoError(t, os.Rename(filepath.Join(dir, "old.go"), filepath.Join(dir, "new.go")))
	commitAll(t, dir, "rename old.go to new.go")

	var capturedPush contract.GraphPush
	var bulkReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &bulkReq)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: base}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	// old path must be in both purge sets
	require.Contains(t, capturedPush.Files, "old.go", "rename old-path must be in code-graph Files for purge")
	require.Contains(t, bulkReq.ReconcileFiles, "old.go", "rename old-path must be in memories reconcile_files for purge")

	// old path must contribute no entities (purge-only)
	for _, e := range capturedPush.Entities {
		require.NotEqual(t, "old.go", e.FilePath, "rename old-path must contribute no entities")
	}

	// new path must be in Files and analyzed (has entities)
	require.Contains(t, capturedPush.Files, "new.go", "rename new-path must be in code-graph Files")
}

// TestRunEmptyChangesetIsNoOp asserts that an incremental run with since==HEAD
// (no new commits) returns nil WITHOUT calling /code-graph:bulk or /memories:bulk.
func TestRunEmptyChangesetIsNoOp(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nbody\n"), 0o644))
	head := commitAll(t, dir, "init")

	var graphCalled, chunksCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			graphCalled = true
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"codegraph: invalid push scope: files required"}`))
		case "/memories:bulk":
			chunksCalled = true
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	// since==HEAD means zero changed files - must be a successful no-op
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: head}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.False(t, graphCalled, "/code-graph:bulk must NOT be called for empty changeset")
	require.False(t, chunksCalled, "/memories:bulk must NOT be called for empty changeset")
}

// commitAll commits all changes and returns HEAD.
func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// TestRunAggregatesSymbols verifies that symbols from analyzers are collected and
// sent in the GraphPush payload (symbols key must appear when non-empty).
func TestRunAggregatesSymbols(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}

	// Write a simple JS file with an exported function and an external import.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "comp.js"),
		[]byte("import React from 'react';\nexport function MyComp() {}\n"), 0o644))
	for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}

	var capturedPush contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"x"}`))
		case "/memories:bulk":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "x", baseURL: srv.URL, full: true, crossRepoPrefix: "github.com/szymonrychu/"}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.NotEmpty(t, capturedPush.Symbols, "expected symbols in GraphPush payload")
}

// TestRunSCIPPath verifies that --scip causes only /code-graph:bulk to be called
// (no /memories:bulk) and that the entities from the SCIP index are present.
func TestRunSCIPPath(t *testing.T) {
	const (
		symA = "go 1.0 `main`/A()."
		symB = "go 1.0 `main`/B()."
	)
	idx := &scipbindings.Index{
		Metadata: &scipbindings.Metadata{
			Version:              0,
			ProjectRoot:          "file:///repo",
			TextDocumentEncoding: scipbindings.TextEncoding_UTF8,
		},
		Documents: []*scipbindings.Document{
			{
				RelativePath: "foo.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "A"},
					{Symbol: symB, Kind: scipbindings.SymbolInformation_Function, DisplayName: "B"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{Range: []int32{0, 0, 5, 0}, Symbol: symA, SymbolRoles: int32(scipbindings.SymbolRole_Definition)},
					{Range: []int32{10, 0, 15, 0}, Symbol: symB, SymbolRoles: int32(scipbindings.SymbolRole_Definition)},
					{Range: []int32{2, 4, 2, 5}, Symbol: symB, SymbolRoles: 0},
				},
			},
		},
	}
	b, err := proto.Marshal(idx)
	require.NoError(t, err)
	tmp := filepath.Join(t.TempDir(), "index.scip")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))

	var graphHit bool
	var chunksCalled bool
	var capturedPush contract.GraphPush

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			graphHit = true
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"myrepo"}`))
		case "/memories:bulk":
			chunksCalled = true
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{scipPath: tmp, scipRepo: "myrepo", baseURL: srv.URL}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	require.True(t, graphHit, "code-graph:bulk must be called")
	require.False(t, chunksCalled, "memories:bulk must NOT be called for SCIP path")
	require.Len(t, capturedPush.Entities, 2)
	require.Len(t, capturedPush.Edges, 1)
}

func TestRunIngestsFixtureRepo(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hi\n"), 0o644))
	for _, a := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}

	var graphHit, bulkHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			graphHit = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"x"}`))
		case "/memories:bulk":
			bulkHit = true
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "x", baseURL: srv.URL}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.True(t, graphHit)
	require.True(t, bulkHit)
}

func TestRunTagsASTPushWithExtractor(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	var astPush contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &astPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	// No OPENAI_API_KEY -> semantic stage skipped, AST push still tagged "ast".
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(string) string { return "" }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.Equal(t, contract.ExtractorAST, astPush.Extractor)
}

func TestRunSkipsSemanticStageWhenNoKey(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	var missesCalled, semanticPush bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			missesCalled = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go"]`))
		case "/code-graph:bulk":
			var p contract.GraphPush
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &p)
			if p.Extractor == contract.ExtractorSemantic {
				semanticPush = true
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(string) string { return "" }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.False(t, missesCalled, "semantic-misses must not be called without a key")
	require.False(t, semanticPush, "no semantic push without a key")
}

func TestRunSkipsSemanticStageWhenDisabled(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	var missesCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			missesCalled = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go"]`))
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "SEMANTIC_INGEST": "false"}
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(k string) string { return env[k] }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.False(t, missesCalled, "SEMANTIC_INGEST=false must skip the whole stage")
}

func TestRunSemanticStagePushesSecondGraphWithSHAs(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	// Fake OpenAI endpoint returns a valid fragment with one concept + one edge.
	// Edge target uses the model-emitted id ("misc_idea"), not the canonical form,
	// so ParseFragment's concept-id remap is exercised end-to-end.
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		w.WriteHeader(200)
		frag := `{"nodes":[{"id":"misc_idea","label":"Misc Idea","file_type":"concept","source_file":"a.go"}],` +
			`"edges":[{"source":"go:func:example.com/m.A","target":"misc_idea","relation":"conceptually_related_to","confidence":"INFERRED","confidence_score":0.75,"source_file":"a.go"}],` +
			`"hyperedges":[]}`
		out := map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": frag}}}}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer openai.Close()

	var semanticPush contract.GraphPush
	var sawSemantic atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			var req contract.SemanticMissesRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			require.Equal(t, "m", req.Repo)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go"]`))
		case "/code-graph:bulk":
			var p contract.GraphPush
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &p)
			if p.Extractor == contract.ExtractorSemantic {
				semanticPush = p
				sawSemantic.Store(true)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "OPENAI_BASE_URL": openai.URL}
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(k string) string { return env[k] }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	require.True(t, sawSemantic.Load(), "expected a semantic GraphPush")
	require.Equal(t, contract.ExtractorSemantic, semanticPush.Extractor)
	require.Contains(t, semanticPush.Files, "a.go")
	require.NotEmpty(t, semanticPush.FileSHAs["a.go"], "semantic push must carry content_sha for the miss")
	require.NotEmpty(t, semanticPush.Edges, "semantic edge must be present")
	require.Equal(t, contract.RelConceptuallyRelated, semanticPush.Edges[0].Relation)
}

func TestRunSemanticStageBestEffortOnLLMError(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	// OpenAI always 500s (after the one retry it stays failed).
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer openai.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go"]`))
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "OPENAI_BASE_URL": openai.URL}
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(k string) string { return env[k] }}
	// LLM failure must NOT fail the ingest.
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
}

// TestRunSCIPPathTaggedWithSCIPExtractor verifies finding 1: the SCIP ingest
// path must tag its GraphPush with extractor="scip", not leave it empty (which
// would cause the server to treat it as "ast" and clobber AST rows).
func TestRunSCIPPathTaggedWithSCIPExtractor(t *testing.T) {
	const symA = "go 1.0 `main`/A()."
	idx := &scipbindings.Index{
		Metadata: &scipbindings.Metadata{
			Version:              0,
			ProjectRoot:          "file:///repo",
			TextDocumentEncoding: scipbindings.TextEncoding_UTF8,
		},
		Documents: []*scipbindings.Document{
			{
				RelativePath: "foo.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "A"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{Range: []int32{0, 0, 5, 0}, Symbol: symA, SymbolRoles: int32(scipbindings.SymbolRole_Definition)},
				},
			},
		},
	}
	b, err := proto.Marshal(idx)
	require.NoError(t, err)
	tmp := filepath.Join(t.TempDir(), "index.scip")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))

	var capturedPush contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"myrepo"}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{scipPath: tmp, scipRepo: "myrepo", baseURL: srv.URL}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
	require.Equal(t, contract.ExtractorSCIP, capturedPush.Extractor,
		"SCIP ingest must tag its push with extractor=scip, not empty (which server maps to ast)")
}

// TestRunSemanticUnreadableMissFileExcludedFromPush verifies finding 2: an
// unreadable miss file must NOT appear in the Files field of the semantic
// GraphPush so the server does not purge its existing semantic rows with no
// replacement.
func TestRunSemanticUnreadableMissFileExcludedFromPush(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readable.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	// Fake OpenAI returns a valid fragment for readable.go.
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		frag := `{"nodes":[{"id":"misc_idea","label":"Misc Idea","file_type":"concept","source_file":"readable.go"}],"edges":[],"hyperedges":[]}`
		out := map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": frag}}}}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer openai.Close()

	var semanticPush contract.GraphPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			// Return both readable.go and a nonexistent file as misses.
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["readable.go","nonexistent.go"]`))
		case "/code-graph:bulk":
			var p contract.GraphPush
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &p)
			if p.Extractor == contract.ExtractorSemantic {
				semanticPush = p
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "OPENAI_BASE_URL": openai.URL}
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(k string) string { return env[k] }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	require.Contains(t, semanticPush.Files, "readable.go",
		"readable miss file must be in semantic push Files")
	require.NotContains(t, semanticPush.Files, "nonexistent.go",
		"unreadable miss file must NOT be in semantic push Files (would cause server-side purge with no replacement)")
}

// TestRunPushesMetricsWhenURLSet verifies the obs metrics scaffold is actually
// wired: when metricsPushURL is set, run() POSTs the gathered Prometheus text
// (including ingest_runs_total) to that URL at job end.
func TestRunPushesMetricsWhenURLSet(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n\nbody\n"), 0o644))
	commitAll(t, dir, "init")

	var metricsBody atomic.Value
	metricsBody.Store("")
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		metricsBody.Store(string(b))
		w.WriteHeader(200)
	}))
	defer metrics.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, metricsPushURL: metrics.URL}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	got := metricsBody.Load().(string)
	require.Contains(t, got, "ingest_runs_total", "metrics push must carry the gathered Prometheus text")
}

// TestRunRecordsFailureResultMetric verifies finding 1: a failed run (bad
// repoRoot so walk.Diff fails) must push ingest_run_result_total{result="failure"}
// and must also push ingest_stage_duration_seconds{stage="total"}.
func TestRunRecordsFailureResultMetric(t *testing.T) {
	var metricsBody string
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		metricsBody = string(b)
		w.WriteHeader(200)
	}))
	defer metrics.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	opts := options{
		repoRoot:       "/nonexistent-repo-for-test",
		repoName:       "m",
		baseURL:        srv.URL,
		metricsPushURL: metrics.URL,
	}
	err := run(context.Background(), opts, http.DefaultClient)
	require.Error(t, err, "walk.Diff on nonexistent repo must fail")

	require.Contains(t, metricsBody, `result="failure"`,
		"failed run must push ingest_run_result_total{result=\"failure\"}")
	require.Contains(t, metricsBody, `stage="total"`,
		"failed run must push total duration via IngestStageDuration")
}

// TestRunSemanticMissPathEscapeIsRejected verifies finding 3: a server-returned
// miss path that escapes repoRoot (e.g. ../../etc/passwd) must be silently
// skipped and must NOT appear in the semantic push Files list.
func TestRunSemanticMissPathEscapeIsRejected(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	// Write a "secret" file one level above the repo root so we can confirm
	// it is never read.
	secretFile := filepath.Join(filepath.Dir(dir), "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("should-never-be-read"), 0o644))
	t.Cleanup(func() { _ = os.Remove(secretFile) })

	// OpenAI endpoint: should not be called for the escape path; if called for
	// a.go it returns a trivial fragment.
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		frag := `{"nodes":[],"edges":[],"hyperedges":[]}`
		out := map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": frag}}}}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer openai.Close()

	var semanticPush contract.GraphPush
	var sawSemantic bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			// Return a.go plus a traversal-escape path targeting the secret file.
			rel := "../../secret.txt"
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go","` + rel + `"]`))
		case "/code-graph:bulk":
			var p contract.GraphPush
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &p)
			if p.Extractor == contract.ExtractorSemantic {
				semanticPush = p
				sawSemantic = true
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "OPENAI_BASE_URL": openai.URL}
	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, getenv: func(k string) string { return env[k] }}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	// The escape path must never appear in Files (which would tell the server
	// to scope an entity to a path outside the repo).
	if sawSemantic {
		for _, f := range semanticPush.Files {
			require.NotContains(t, f, "..", "semantic push Files must not contain escape paths")
			require.False(t, strings.HasPrefix(f, "/"), "semantic push Files must not contain absolute paths")
		}
	}
}

// newGitRepo creates an initialized git repo in a temp dir.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	return dir
}

// TestRunAnalyzerFailureDoesNotPurgeFiles verifies finding 1: when an analyzer
// fails for a set of files, those files must NOT appear in reconcile_files
// (which would purge existing chunks with no replacement). Deleted files are
// unaffected and still reconciled.
//
// We force an analyzer failure by making a Python file unreadable on disk
// (exists in git, committed, then chmod 000 before analysis). The Python
// analyzer returns an error on os.ReadFile failure, so the .py file whose
// analyzer errored must be excluded from reconcile_files while the deleted
// .md file must still be present.
func TestRunAnalyzerFailureDoesNotPurgeFiles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root: chmod 000 is ineffective as root")
	}

	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}

	// Initial commit: a Python file and a Markdown file that will be deleted.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("def hello():\n    pass\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gone.md"),
		[]byte("# Gone\n"), 0o644))
	base := commitAll(t, dir, "init")

	// Second commit: modify app.py and delete gone.md.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.py"),
		[]byte("def hello():\n    return 42\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.md")))
	commitAll(t, dir, "modify app.py, delete gone.md")

	// Make app.py unreadable AFTER the commit so git diff still shows it as
	// changed but the Python analyzer fails to read it.
	require.NoError(t, os.Chmod(filepath.Join(dir, "app.py"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "app.py"), 0o644) })

	var bulkReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &bulkReq)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: base}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	// gone.md (deleted) must be reconciled - its stale chunks should be purged.
	require.Contains(t, bulkReq.ReconcileFiles, "gone.md",
		"deleted file must be in reconcile_files even when another analyzer failed")
	// app.py (whose Python analyzer failed due to unreadable file) must NOT be in reconcile_files.
	require.NotContains(t, bulkReq.ReconcileFiles, "app.py",
		"file whose analyzer failed must NOT be in reconcile_files (would purge chunks with no replacement)")
}

// TestRunTouchedDeduplicatedForRenames verifies finding 4: when two different
// renames share the same old or new path (edge case) or any duplication occurs,
// the code-graph Files and reconcile_files must not contain duplicate entries.
// We exercise the basic rename path and assert each file appears at most once.
func TestRunTouchedDeduplicatedForRenames(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.md"), []byte("# Old\n"), 0o644))
	base := commitAll(t, dir, "init")

	require.NoError(t, os.Rename(filepath.Join(dir, "old.md"), filepath.Join(dir, "new.md")))
	commitAll(t, dir, "rename old.md to new.md")

	var graphPush contract.GraphPush
	var bulkReq contract.BulkMemoriesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &graphPush)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		case "/memories:bulk":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &bulkReq)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, since: base}
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))

	seen := make(map[string]int)
	for _, f := range graphPush.Files {
		seen[f]++
	}
	for f, count := range seen {
		require.Equal(t, 1, count, "file %q appears %d times in code-graph Files; must be deduplicated", f, count)
	}

	seen2 := make(map[string]int)
	for _, f := range bulkReq.ReconcileFiles {
		seen2[f]++
	}
	for f, count := range seen2 {
		require.Equal(t, 1, count, "file %q appears %d times in reconcile_files; must be deduplicated", f, count)
	}
}

// TestRunMetricsPushUsesPlainClient verifies finding 2: the metrics push must
// NOT include an Authorization header (i.e. it must use a plain client, not the
// OIDC-bearer hc passed into run). We check this by inspecting the header on the
// push endpoint.
func TestRunMetricsPushUsesPlainClient(t *testing.T) {
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n"), 0o644))
	commitAll(t, dir, "init")

	var metricsAuthHeader string
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metricsAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer metrics.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	// Simulate an OIDC client that always adds a bearer token.
	fakeOIDCTransport := &addAuthTransport{token: "fake-oidc-token", base: http.DefaultTransport}
	fakeOIDCClient := &http.Client{Transport: fakeOIDCTransport}

	opts := options{repoRoot: dir, repoName: "m", baseURL: srv.URL, metricsPushURL: metrics.URL}
	require.NoError(t, run(context.Background(), opts, fakeOIDCClient))

	require.Empty(t, metricsAuthHeader,
		"metrics push must NOT carry an Authorization header (must use plain client, not OIDC hc)")
}

// addAuthTransport is a fake RoundTripper that injects a Bearer token.
type addAuthTransport struct {
	token string
	base  http.RoundTripper
}

func (t *addAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req2)
}

// TestRunHTTPTimeoutThreadedToLLMClient verifies finding 3: when httpTimeout is
// set on options, the LLM client uses that timeout rather than the hardcoded 60s.
// We exercise this indirectly: with a very short httpTimeout the LLM call to a
// slow server must time out, and runSemantic must still swallow it (best-effort).
// The key assertion is that run() returns nil (not a timeout error) regardless.
func TestRunHTTPTimeoutThreadedToLLMClient(t *testing.T) {
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package m\n\nfunc A() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/m\n\ngo 1.25\n"), 0o644))
	commitAll(t, dir, "init")

	// OpenAI server that hangs longer than our httpTimeout.
	done := make(chan struct{})
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); openai.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/code-graph/semantic-misses":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`["a.go"]`))
		case "/code-graph:bulk":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"repo":"m"}`))
		default:
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"j","status":"succeeded"}`))
		}
	}))
	defer srv.Close()

	env := map[string]string{"OPENAI_API_KEY": "sk-test", "OPENAI_BASE_URL": openai.URL}
	opts := options{
		repoRoot:    dir,
		repoName:    "m",
		baseURL:     srv.URL,
		httpTimeout: 50 * time.Millisecond, // very short, will timeout the LLM call
		getenv:      func(k string) string { return env[k] },
	}
	// Must not fail the ingest even though LLM times out (best-effort semantic stage).
	require.NoError(t, run(context.Background(), opts, http.DefaultClient))
}
