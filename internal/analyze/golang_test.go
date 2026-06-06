package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func findEdge(edges []contract.Edge, rel, from, to string) (contract.Edge, bool) {
	for _, e := range edges {
		if e.Relation == rel && e.From == from && e.To == to {
			return e, true
		}
	}
	return contract.Edge{}, false
}

func TestGoAnalyzer(t *testing.T) {
	a := analyze.NewGo()
	require.True(t, a.Match("pkg/pkg.go"))
	require.False(t, a.Match("README.md"))

	res, err := a.Analyze(context.Background(), "testdata/go", []string{"pkg/pkg.go"})
	require.NoError(t, err)

	ids := map[string]contract.Entity{}
	for _, e := range res.Entities {
		ids[e.ID] = e
	}
	require.Contains(t, ids, "go:func:example.com/sample/pkg.F")
	require.Contains(t, ids, "go:func:example.com/sample/pkg.G")

	call, ok := findEdge(res.Edges, contract.RelCalls,
		"go:func:example.com/sample/pkg.F", "go:func:example.com/sample/pkg.G")
	require.True(t, ok, "expected F->G calls edge")
	require.Equal(t, contract.ResTypeResolved, call.Properties["resolution"])
	require.Equal(t, "0.98", call.Properties["confidence"])

	// (a) F's entity must carry a repo-relative FilePath.
	fEntity := ids["go:func:example.com/sample/pkg.F"]
	require.Equal(t, "pkg/pkg.go", fEntity.FilePath, "FilePath must be repo-relative")

	// (b) When files = ["pkg/pkg.go"], H (other.go) must be absent; every
	// emitted entity's FilePath and every edge's SrcFile must be in the files set.
	filesScope := map[string]bool{"pkg/pkg.go": true}

	require.NotContains(t, ids, "go:func:example.com/sample/pkg.H",
		"H lives in other.go which is out of scope; must not be emitted")

	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue // package-level entities have no FilePath
		}
		require.True(t, filesScope[e.FilePath],
			"entity %q has FilePath %q not in files set", e.ID, e.FilePath)
	}

	for _, e := range res.Edges {
		if e.SrcFile == "" {
			continue
		}
		require.True(t, filesScope[e.SrcFile],
			"edge %q->%q has SrcFile %q not in files set", e.From, e.To, e.SrcFile)
	}

	for _, c := range res.Chunks {
		require.True(t, filesScope[c.FilePath],
			"chunk for entity %q has FilePath %q not in files set", c.EntityID, c.FilePath)
	}
}
