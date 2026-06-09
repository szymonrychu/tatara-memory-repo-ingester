package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type fakeAnalyzer struct {
	name  string
	match func(string) bool
}

func (f fakeAnalyzer) Name() string        { return f.name }
func (f fakeAnalyzer) Match(p string) bool { return f.match(p) }
func (f fakeAnalyzer) Analyze(_ context.Context, _ string, files []string) (analyze.Result, error) {
	return analyze.Result{Entities: []contract.Entity{{ID: f.name, Name: f.name}}}, nil
}

func TestResultCarriesHyperedges(t *testing.T) {
	var r analyze.Result
	r.Hyperedges = append(r.Hyperedges, contract.Hyperedge{
		ID: "he:1", Label: "l", Relation: "form", SrcFile: "a.go",
		Members: []string{"x", "y", "z"},
	})
	require.Len(t, r.Hyperedges, 1)
	require.Equal(t, "he:1", r.Hyperedges[0].ID)
}

func TestRegistryGroupsByFirstMatch(t *testing.T) {
	reg := analyze.NewRegistry()
	reg.Register(fakeAnalyzer{name: "go", match: func(p string) bool { return p == "a.go" }})
	reg.Register(fakeAnalyzer{name: "docs", match: func(string) bool { return true }}) // catch-all, lower precedence

	groups := reg.Group([]string{"a.go", "README.md"})
	require.Equal(t, []string{"a.go"}, groups["go"])
	require.Equal(t, []string{"README.md"}, groups["docs"])
}

func TestRegistryUnmatchedFileDropped(t *testing.T) {
	reg := analyze.NewRegistry()
	reg.Register(fakeAnalyzer{name: "go", match: func(p string) bool { return p == "a.go" }})
	groups := reg.Group([]string{"a.go", "weird.xyz"})
	require.Equal(t, map[string][]string{"go": {"a.go"}}, groups)
}
