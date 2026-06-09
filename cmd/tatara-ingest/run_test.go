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
	"testing"

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
