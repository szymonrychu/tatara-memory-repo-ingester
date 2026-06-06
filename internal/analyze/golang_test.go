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
}
