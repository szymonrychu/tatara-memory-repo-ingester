package scip_test

import (
	"os"
	"path/filepath"
	"testing"

	scipbindings "github.com/scip-code/scip/bindings/go/scip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/scip"
)

// TestParseBasic builds a minimal SCIP index in-memory (no binary fixture),
// marshals it to a temp file, then asserts Parse returns the expected entities,
// edge, and files list.
//
// Symbol A is a function defined on lines 0-5, symbol B is a function defined
// on lines 10-15. A reference occurrence to B lives at line 2 (inside A's
// range). We expect:
//   - two entities: scip:go:<symA> and scip:go:<symB>, both with FilePath "foo.go"
//   - one edge from scip:go:<symA> -> scip:go:<symB>, relation "calls"
//   - Files == ["foo.go"]
func TestParseBasic(t *testing.T) {
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
					{
						Symbol:      symA,
						Kind:        scipbindings.SymbolInformation_Function,
						DisplayName: "A",
					},
					{
						Symbol:      symB,
						Kind:        scipbindings.SymbolInformation_Function,
						DisplayName: "B",
					},
				},
				Occurrences: []*scipbindings.Occurrence{
					// Definition of A: lines 0-5
					{
						Range:       []int32{0, 0, 5, 0},
						Symbol:      symA,
						SymbolRoles: int32(scipbindings.SymbolRole_Definition),
					},
					// Definition of B: lines 10-15
					{
						Range:       []int32{10, 0, 15, 0},
						Symbol:      symB,
						SymbolRoles: int32(scipbindings.SymbolRole_Definition),
					},
					// Reference to B at line 2 (inside A's range 0-5)
					{
						Range:       []int32{2, 4, 2, 5},
						Symbol:      symB,
						SymbolRoles: 0, // reference (no Definition bit)
					},
				},
			},
		},
	}

	b, err := proto.Marshal(idx)
	require.NoError(t, err)

	tmp := filepath.Join(t.TempDir(), "index.scip")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))

	gp, err := scip.Parse(tmp, "myrepo")
	require.NoError(t, err)

	// Files
	assert.Equal(t, []string{"foo.go"}, gp.Files)

	// Entities
	byID := make(map[string]struct{})
	for _, e := range gp.Entities {
		byID[e.ID] = struct{}{}
		assert.Equal(t, "foo.go", e.FilePath)
	}
	assert.Contains(t, byID, "scip:go:"+symA)
	assert.Contains(t, byID, "scip:go:"+symB)

	// Edge A -> B, relation "calls"
	require.Len(t, gp.Edges, 1)
	edge := gp.Edges[0]
	assert.Equal(t, "scip:go:"+symA, edge.From)
	assert.Equal(t, "scip:go:"+symB, edge.To)
	assert.Equal(t, "calls", edge.Relation)
	assert.Equal(t, "foo.go", edge.SrcFile)
	assert.Equal(t, "type_resolved", edge.Properties["resolution"])
	assert.Equal(t, "0.98", edge.Properties["confidence"])

	// LineStart/LineEnd must be set from the definition occurrence Range.
	// SCIP ranges are 0-based; we store 1-based line numbers (LineStart = range[0]+1).
	for _, e := range gp.Entities {
		switch e.ID {
		case "scip:go:" + symA:
			// Definition A: Range [0,0,5,0] -> LineStart=1, LineEnd=5
			assert.Equal(t, 1, e.LineStart, "entity A LineStart must be 1-based start line")
			assert.Equal(t, 5, e.LineEnd, "entity A LineEnd must be end line")
		case "scip:go:" + symB:
			// Definition B: Range [10,0,15,0] -> LineStart=11, LineEnd=15
			assert.Equal(t, 11, e.LineStart, "entity B LineStart must be 1-based start line")
			assert.Equal(t, 15, e.LineEnd, "entity B LineEnd must be end line")
		}
	}
}
