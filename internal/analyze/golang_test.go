package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func findSymbol(syms []contract.SymbolRow, role, symbol string) (contract.SymbolRow, bool) {
	for _, s := range syms {
		if s.Role == role && s.Symbol == symbol {
			return s, true
		}
	}
	return contract.SymbolRow{}, false
}

func findEdge(edges []contract.Edge, rel, from, to string) (contract.Edge, bool) {
	for _, e := range edges {
		if e.Relation == rel && e.From == from && e.To == to {
			return e, true
		}
	}
	return contract.Edge{}, false
}

// TestClassifyRef tests the pure helper via the exported shim.
func TestClassifyRef(t *testing.T) {
	cases := []struct {
		name        string
		objPkgPath  string
		modulePath  string
		prefix      string
		wantEmit    bool
		wantSymbol  string
	}{
		{
			name: "in-module ref",
			objPkgPath: "example.com/sample/pkg", modulePath: "example.com/sample",
			prefix: "github.com/szymonrychu/", wantEmit: false,
		},
		{
			name: "external under prefix",
			objPkgPath: "github.com/szymonrychu/other/pkg", modulePath: "example.com/sample",
			prefix: "github.com/szymonrychu/", wantEmit: true,
			wantSymbol: "github.com/szymonrychu/other/pkg.DoThing",
		},
		{
			name: "stdlib - no emit",
			objPkgPath: "fmt", modulePath: "example.com/sample",
			prefix: "github.com/szymonrychu/", wantEmit: false,
		},
		{
			name: "third-party no prefix match",
			objPkgPath: "github.com/some-other/lib", modulePath: "example.com/sample",
			prefix: "github.com/szymonrychu/", wantEmit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emit, sym := analyze.ClassifyRef(tc.objPkgPath, "DoThing", tc.modulePath, tc.prefix)
			require.Equal(t, tc.wantEmit, emit)
			if tc.wantEmit {
				require.Equal(t, tc.wantSymbol, sym)
			}
		})
	}
}

func TestGoAnalyzer(t *testing.T) {
	a := analyze.NewGo("github.com/szymonrychu/")
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

	// (c) provides SymbolRow for exported func F.
	provF, ok := findSymbol(res.Symbols, contract.RoleProvides, "example.com/sample/pkg.F")
	require.True(t, ok, "expected provides SymbolRow for F")
	require.Equal(t, "go", provF.Lang)
	require.Equal(t, "func", provF.Kind)
	require.Equal(t, "go:func:example.com/sample/pkg.F", provF.EntityID)
	require.Equal(t, "pkg/pkg.go", provF.SrcFile)

	// exported func G is also in scope.
	provG, okG := findSymbol(res.Symbols, contract.RoleProvides, "example.com/sample/pkg.G")
	require.True(t, okG, "expected provides SymbolRow for G")
	require.Equal(t, "pkg/pkg.go", provG.SrcFile)

	// all SymbolRow.SrcFile values must be within the files scope.
	for _, s := range res.Symbols {
		require.True(t, filesScope[s.SrcFile],
			"SymbolRow %q has SrcFile %q not in files set", s.Symbol, s.SrcFile)
	}
}
