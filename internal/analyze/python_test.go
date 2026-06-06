package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestPythonAnalyzer(t *testing.T) {
	a := analyze.NewPython()
	require.True(t, a.Match("pkg/mod.py"))

	res, err := a.Analyze(context.Background(), "testdata/py", []string{"pkg/mod.py"})
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}
	require.True(t, ids["py:func:pkg.mod.f"])
	require.True(t, ids["py:func:pkg.mod.g"])

	call, ok := findEdge(res.Edges, contract.RelCalls, "py:func:pkg.mod.f", "py:func:pkg.mod.g")
	require.True(t, ok, "expected f->g calls edge")
	require.Equal(t, contract.ResScopedNameMatch, call.Properties["resolution"])
	require.Equal(t, "0.85", call.Properties["confidence"])

	// len([]) is a builtin: no edge, recorded as a dangling call on f.
	_, hasLen := findEdge(res.Edges, contract.RelCalls, "py:func:pkg.mod.f", "py:func:pkg.mod.len")
	require.False(t, hasLen)
}
