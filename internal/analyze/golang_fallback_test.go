package analyze_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// TestGoFallbackBrokenPackage asserts that a package that fails go/packages type-checking
// still yields go_func entities and a degraded calls edge via the tree-sitter fallback.
//
// RED: the current code skips packages with errors -> zero entities.
// GREEN: the fallback emits H and G as go_func entities plus a H->G calls edge
//
//	with degraded_by=no_typecheck and confidence <= 0.45.
func TestGoFallbackBrokenPackage(t *testing.T) {
	a := analyze.NewGo("github.com/szymonrychu/")

	files := []string{"pkg/broken.go"}
	res, err := a.Analyze(context.Background(), "testdata/go_broken", files)
	require.NoError(t, err)

	// Build entity ID map.
	entityIDs := map[string]contract.Entity{}
	for _, e := range res.Entities {
		entityIDs[e.ID] = e
	}

	// pkgPath = "example.com/broken/pkg" (modulePath + "/pkg")
	const (
		hID = "go:func:example.com/broken/pkg.H"
		gID = "go:func:example.com/broken/pkg.G"
	)

	hEnt, hOK := entityIDs[hID]
	require.True(t, hOK, "expected go_func entity for H; entities: %v", entityIDKeys(entityIDs))
	require.Equal(t, contract.EntityGoFunc, hEnt.Type)
	require.Equal(t, "pkg/broken.go", hEnt.FilePath, "FilePath must be repo-relative")

	gEnt, gOK := entityIDs[gID]
	require.True(t, gOK, "expected go_func entity for G; entities: %v", entityIDKeys(entityIDs))
	require.Equal(t, contract.EntityGoFunc, gEnt.Type)
	require.Equal(t, "pkg/broken.go", gEnt.FilePath)

	// H -> G calls edge with degraded_by=no_typecheck and confidence <= 0.45.
	callEdge, edgeOK := findEdge(res.Edges, contract.RelCalls, hID, gID)
	require.True(t, edgeOK, "expected H->G calls edge; edges: %v", edgeSummary(res.Edges))

	degradedBy, hasDegradedBy := callEdge.Properties["degraded_by"]
	require.True(t, hasDegradedBy, "calls edge must have degraded_by property")
	require.Contains(t, degradedBy, "no_typecheck")

	confStr, hasConf := callEdge.Properties["confidence"]
	require.True(t, hasConf, "calls edge must have confidence property")
	conf, err2 := strconv.ParseFloat(confStr, 64)
	require.NoError(t, err2, "confidence must be parseable float")
	require.LessOrEqual(t, conf, 0.45, "confidence must be <= 0.45 for fallback calls")

	// All emitted entities/edges/chunks must be within the files scope.
	filesScope := map[string]bool{"pkg/broken.go": true}
	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue
		}
		require.True(t, filesScope[e.FilePath],
			"entity %q FilePath %q not in scope", e.ID, e.FilePath)
	}
	for _, e := range res.Edges {
		if e.SrcFile == "" {
			continue
		}
		require.True(t, filesScope[e.SrcFile],
			"edge %q->%q SrcFile %q not in scope", e.From, e.To, e.SrcFile)
	}
	for _, c := range res.Chunks {
		require.True(t, filesScope[c.FilePath],
			"chunk %q FilePath %q not in scope", c.EntityID, c.FilePath)
	}
}

// entityIDKeys returns the keys of an entity ID map for error messages.
func entityIDKeys(m map[string]contract.Entity) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// edgeSummary returns a short string list of edges for error messages.
func edgeSummary(edges []contract.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.From+"->"+e.To+"("+e.Relation+")")
	}
	return out
}
