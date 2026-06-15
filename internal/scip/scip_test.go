package scip_test

import (
	"os"
	"path/filepath"
	"testing"

	scipbindings "github.com/scip-code/scip/bindings/go/scip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/scip"
)

// helper: marshal idx to a temp file, return its path.
func writeIndex(t *testing.T, idx *scipbindings.Index) string {
	t.Helper()
	b, err := proto.Marshal(idx)
	require.NoError(t, err)
	tmp := filepath.Join(t.TempDir(), "index.scip")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))
	return tmp
}

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
					// Definition of A: name-token Range [0,0,0,1]; EnclosingRange [0,0,5,0]
					{
						Range:          []int32{0, 0, 0, 1},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         symA,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Definition of B: name-token Range [10,0,10,1]; EnclosingRange [10,0,15,0]
					{
						Range:          []int32{10, 0, 10, 1},
						EnclosingRange: []int32{10, 0, 15, 0},
						Symbol:         symB,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Reference to B at line 2 (inside A's EnclosingRange 0-5)
					{
						Range:       []int32{2, 4, 2, 5},
						Symbol:      symB,
						SymbolRoles: 0, // reference (no Definition bit)
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "myrepo")
	require.NoError(t, err)

	// Files
	assert.Equal(t, []string{"foo.go"}, gp.Files)

	// Entities
	byID := make(map[string]contract.Entity)
	for _, e := range gp.Entities {
		byID[e.ID] = e
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

	// Finding 3: typed confidence fields must be set.
	assert.InDelta(t, 0.98, edge.ConfidenceScore, 1e-9, "ConfidenceScore must be 0.98")
	assert.Equal(t, contract.TierInferred, edge.ConfidenceTier, "ConfidenceTier must be INFERRED")

	// Finding 7: LineStart/LineEnd must be set from EnclosingRange (preferred) or Range.
	// EnclosingRange for A = [0,0,5,0] -> LineStart=1 (0-based start+1), LineEnd=5
	eA := byID["scip:go:"+symA]
	assert.Equal(t, 1, eA.LineStart, "entity A LineStart must be 1-based start line")
	assert.Equal(t, 5, eA.LineEnd, "entity A LineEnd must be end line from EnclosingRange")

	// EnclosingRange for B = [10,0,15,0] -> LineStart=11, LineEnd=15
	eB := byID["scip:go:"+symB]
	assert.Equal(t, 11, eB.LineStart, "entity B LineStart must be 1-based start line")
	assert.Equal(t, 15, eB.LineEnd, "entity B LineEnd must be end line from EnclosingRange")
}

// TestEnclosingRangeUsedForContainment verifies finding 1: enclosingDef must use
// EnclosingRange (the function body) not Range (the name token) for containment.
// A real SCIP indexer emits a name-token Range (single line) + EnclosingRange
// (the full body). A reference inside the body must produce an edge; a reference
// between functions must not.
func TestEnclosingRangeUsedForContainment(t *testing.T) {
	const (
		symA = "go 1.0 `main`/FuncA()."
		symB = "go 1.0 `main`/FuncB()."
	)
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "main.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "FuncA"},
					{Symbol: symB, Kind: scipbindings.SymbolInformation_Function, DisplayName: "FuncB"},
				},
				Occurrences: []*scipbindings.Occurrence{
					// FuncA definition: name token on line 0; body lines 0-9.
					{
						Range:          []int32{0, 5, 0, 10}, // name token (single line)
						EnclosingRange: []int32{0, 0, 9, 1},  // body span
						Symbol:         symA,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// FuncB definition: name token on line 12; body lines 12-20.
					{
						Range:          []int32{12, 5, 12, 10},
						EnclosingRange: []int32{12, 0, 20, 1},
						Symbol:         symB,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Reference to FuncB at line 5 (inside FuncA body lines 0-9).
					{
						Range:       []int32{5, 2, 5, 7},
						Symbol:      symB,
						SymbolRoles: 0,
					},
					// Reference to FuncB at line 11 (gap between functions, should not match FuncA).
					{
						Range:       []int32{11, 0, 11, 5},
						Symbol:      symB,
						SymbolRoles: 0,
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)

	// Only one edge expected: FuncA -> FuncB (line 5 inside FuncA body).
	// Line 11 reference must be dropped (outside any body).
	require.Len(t, gp.Edges, 1, "expected exactly one edge: line-5 ref inside FuncA body")
	assert.Equal(t, "scip:go:"+symA, gp.Edges[0].From)
	assert.Equal(t, "scip:go:"+symB, gp.Edges[0].To)
}

// TestHalfOpenRangeExclusiveEnd verifies finding 2: a reference on the line
// immediately following a function body must NOT be attributed to that function.
func TestHalfOpenRangeExclusiveEnd(t *testing.T) {
	const (
		symA = "go 1.0 `main`/G()."
		symB = "go 1.0 `main`/H()."
	)
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "g.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "G"},
					{Symbol: symB, Kind: scipbindings.SymbolInformation_Function, DisplayName: "H"},
				},
				Occurrences: []*scipbindings.Occurrence{
					// G body: lines 0-4 (EnclosingRange end=4 is exclusive -> last covered line is 3).
					{
						Range:          []int32{0, 5, 0, 6},
						EnclosingRange: []int32{0, 0, 4, 0},
						Symbol:         symA,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// H definition.
					{
						Range:          []int32{5, 5, 5, 6},
						EnclosingRange: []int32{5, 0, 10, 0},
						Symbol:         symB,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Reference to H at line 4: half-open end of G -> must NOT belong to G.
					{
						Range:       []int32{4, 0, 4, 1},
						Symbol:      symB,
						SymbolRoles: 0,
					},
					// Reference to H at line 3: still inside G (lines 0-3 inclusive).
					{
						Range:       []int32{3, 0, 3, 1},
						Symbol:      symB,
						SymbolRoles: 0,
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)

	// Only the line-3 reference should produce an edge (inside G's body 0-3).
	// The line-4 reference is at the exclusive end and must be dropped.
	require.Len(t, gp.Edges, 1, "expected exactly 1 edge (line 3 inside G, line 4 is exclusive end)")
	assert.Equal(t, "scip:go:"+symA, gp.Edges[0].From)
}

// TestConfidenceTypedFields verifies finding 3: ConfidenceScore and ConfidenceTier
// must be set on edges (not just the Properties string).
func TestConfidenceTypedFields(t *testing.T) {
	const (
		symA = "go 1.0 `main`/P()."
		symB = "go 1.0 `main`/Q()."
	)
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "p.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "P"},
					{Symbol: symB, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Q"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						Range:          []int32{0, 5, 0, 6},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         symA,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					{
						Range:          []int32{10, 5, 10, 6},
						EnclosingRange: []int32{10, 0, 15, 0},
						Symbol:         symB,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					{Range: []int32{2, 0, 2, 1}, Symbol: symB, SymbolRoles: 0},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	require.Len(t, gp.Edges, 1)
	e := gp.Edges[0]
	assert.InDelta(t, 0.98, e.ConfidenceScore, 1e-9, "ConfidenceScore must be 0.98 not 0")
	assert.Equal(t, contract.TierInferred, e.ConfidenceTier, "ConfidenceTier must not be empty")
}

// TestSymbolRowsProvides verifies finding 4: definitions emit provides SymbolRows.
func TestSymbolRowsProvides(t *testing.T) {
	const sym = "go 1.0 `pkg`/Foo()."
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "foo.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: sym, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Foo"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						Range:          []int32{0, 5, 0, 8},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         sym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)

	var provides []contract.SymbolRow
	for _, sr := range gp.Symbols {
		if sr.Role == contract.RoleProvides {
			provides = append(provides, sr)
		}
	}
	require.Len(t, provides, 1)
	assert.Equal(t, sym, provides[0].Symbol)
	assert.Equal(t, "go", provides[0].Lang)
	assert.Equal(t, "foo.go", provides[0].SrcFile)
}

// TestSymbolRowsRequires verifies finding 4: references to external symbols emit requires SymbolRows.
func TestSymbolRowsRequires(t *testing.T) {
	const (
		symLocal    = "go 1.0 `pkg`/LocalFn()."
		symExternal = "go 1.0 `otherpkg`/ExternalFn()."
	)
	idx := &scipbindings.Index{
		// ExternalSymbols carries the kind info for the external symbol.
		ExternalSymbols: []*scipbindings.SymbolInformation{
			{Symbol: symExternal, Kind: scipbindings.SymbolInformation_Function, DisplayName: "ExternalFn"},
		},
		Documents: []*scipbindings.Document{
			{
				RelativePath: "local.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symLocal, Kind: scipbindings.SymbolInformation_Function, DisplayName: "LocalFn"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						Range:          []int32{0, 5, 0, 12},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         symLocal,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Reference to external symbol inside LocalFn body.
					{Range: []int32{2, 0, 2, 10}, Symbol: symExternal, SymbolRoles: 0},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)

	// Edge should be emitted (ExternalSymbols provides kind info -> "calls").
	require.Len(t, gp.Edges, 1)
	assert.Equal(t, "calls", gp.Edges[0].Relation, "external function reference must produce calls edge")

	// requires SymbolRow must be present.
	var requires []contract.SymbolRow
	for _, sr := range gp.Symbols {
		if sr.Role == contract.RoleRequires {
			requires = append(requires, sr)
		}
	}
	require.Len(t, requires, 1)
	assert.Equal(t, symExternal, requires[0].Symbol)
}

// TestExternalSymbolsKindLookup verifies finding 5: Index.ExternalSymbols is
// consulted for kind resolution so a reference to an external function emits a
// "calls" edge rather than "references".
func TestExternalSymbolsKindLookup(t *testing.T) {
	const (
		symLocal = "go 1.0 `pkg`/Caller()."
		symExt   = "go 1.0 `ext`/ExternalMethod()."
	)
	idx := &scipbindings.Index{
		ExternalSymbols: []*scipbindings.SymbolInformation{
			{Symbol: symExt, Kind: scipbindings.SymbolInformation_Method, DisplayName: "ExternalMethod"},
		},
		Documents: []*scipbindings.Document{
			{
				RelativePath: "caller.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symLocal, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Caller"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						Range:          []int32{0, 5, 0, 11},
						EnclosingRange: []int32{0, 0, 8, 0},
						Symbol:         symLocal,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					{Range: []int32{3, 0, 3, 14}, Symbol: symExt, SymbolRoles: 0},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	require.Len(t, gp.Edges, 1)
	assert.Equal(t, contract.RelCalls, gp.Edges[0].Relation,
		"reference to external Method must produce 'calls' edge, not 'references'")
}

// TestEdgeDeduplication verifies finding 6: multiple occurrences of the same
// callee inside one enclosing def collapse to a single edge.
func TestEdgeDeduplication(t *testing.T) {
	const (
		symA = "go 1.0 `main`/Dedup()."
		symB = "go 1.0 `main`/Target()."
	)
	occurrences := []*scipbindings.Occurrence{
		{
			Range:          []int32{0, 5, 0, 10},
			EnclosingRange: []int32{0, 0, 20, 0},
			Symbol:         symA,
			SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
		},
		{
			Range:          []int32{21, 5, 21, 11},
			EnclosingRange: []int32{21, 0, 30, 0},
			Symbol:         symB,
			SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
		},
	}
	// Add 5 references to symB inside symA's body.
	for i := int32(2); i <= 6; i++ {
		occurrences = append(occurrences, &scipbindings.Occurrence{
			Range:       []int32{i, 0, i, 6},
			Symbol:      symB,
			SymbolRoles: 0,
		})
	}

	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "dedup.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: symA, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Dedup"},
					{Symbol: symB, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Target"},
				},
				Occurrences: occurrences,
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	assert.Len(t, gp.Edges, 1, "5 occurrences of the same (from,to,relation) must dedup to 1 edge")
}

// TestLineStartEndFromEnclosingRange verifies finding 7: LineStart/LineEnd are
// set from EnclosingRange when available.
func TestLineStartEndFromEnclosingRange(t *testing.T) {
	const sym = "go 1.0 `main`/WithBody()."
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "body.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: sym, Kind: scipbindings.SymbolInformation_Function, DisplayName: "WithBody"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						// Name token on line 5, body spans lines 5-12.
						Range:          []int32{5, 5, 5, 13},
						EnclosingRange: []int32{5, 0, 12, 1},
						Symbol:         sym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	require.Len(t, gp.Entities, 1)
	e := gp.Entities[0]
	// EnclosingRange start=5 (0-based) -> LineStart=6 (1-based)
	assert.Equal(t, 6, e.LineStart, "LineStart should come from EnclosingRange start+1")
	// EnclosingRange end=12 -> LineEnd=12
	assert.Equal(t, 12, e.LineEnd, "LineEnd should come from EnclosingRange end")
}

// TestLineEndNotInvertedForNameTokenRange verifies that a definition with no
// EnclosingRange (only a single-line name-token Range) reports LineEnd >=
// LineStart rather than an inverted span (LineEnd == LineStart-1).
func TestLineEndNotInvertedForNameTokenRange(t *testing.T) {
	const sym = "go 1.0 `main`/NoBody()."
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "nobody.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: sym, Kind: scipbindings.SymbolInformation_Function, DisplayName: "NoBody"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						// Name token only on 0-based line 5; no EnclosingRange.
						Range:       []int32{5, 5, 5, 11},
						Symbol:      sym,
						SymbolRoles: int32(scipbindings.SymbolRole_Definition),
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	require.Len(t, gp.Entities, 1)
	e := gp.Entities[0]
	assert.Equal(t, 6, e.LineStart, "LineStart should be name-token start+1")
	assert.GreaterOrEqual(t, e.LineEnd, e.LineStart, "LineEnd must not be before LineStart")
	assert.Equal(t, 6, e.LineEnd, "single-line def should report LineEnd == LineStart")
}

// TestLastComponentParsesDescriptors verifies finding 8: lastComponent must
// return the descriptor name from ParseSymbol, not a hand-trimmed suffix.
// For a method like "scip-go go pkg 1.0 T#m()." the name is "m", not "T#m".
// Symbols use the canonical SCIP wire format: scheme manager name version descriptors.
func TestLastComponentParsesDescriptors(t *testing.T) {
	// Method sym: last descriptor is m with Method suffix.
	// "scip-go go pkg 1.0 T#m()." -> descriptors: T(Type), m(Method)
	const methSym = "scip-go go pkg 1.0 T#m()."
	// Type sym: last descriptor is T with Type suffix.
	const typeSym = "scip-go go pkg 1.0 T#"

	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "t.go",
				Language:     "go",
				Occurrences: []*scipbindings.Occurrence{
					// No SymbolInformation -> displayName falls back to lastComponent.
					{
						Range:          []int32{0, 5, 0, 6},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         methSym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					{
						Range:          []int32{10, 5, 10, 6},
						EnclosingRange: []int32{10, 0, 15, 0},
						Symbol:         typeSym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)

	nameByID := make(map[string]string)
	for _, e := range gp.Entities {
		nameByID[e.ID] = e.Name
	}

	// Method: descriptor name should be "m", not "T#m" or "T#m()".
	methName := nameByID["scip:go:"+methSym]
	assert.Equal(t, "m", methName, "method symbol name should be 'm', not the full suffix")

	// Type: descriptor name should be "T".
	typeName := nameByID["scip:go:"+typeSym]
	assert.Equal(t, "T", typeName, "type symbol name should be 'T'")
}

// TestRecursionEdge verifies finding 10: a self-recursive call (reference to own
// symbol at a different range) must produce an edge, not be silently dropped.
func TestRecursionEdge(t *testing.T) {
	const sym = "go 1.0 `main`/Recurse()."
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "rec.go",
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: sym, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Recurse"},
				},
				Occurrences: []*scipbindings.Occurrence{
					// Definition of Recurse: name token on line 0, body lines 0-9.
					{
						Range:          []int32{0, 5, 0, 12},
						EnclosingRange: []int32{0, 0, 9, 1},
						Symbol:         sym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
					// Self-reference (recursion) at line 5 - different range from the def.
					{
						Range:       []int32{5, 2, 5, 9},
						Symbol:      sym,
						SymbolRoles: 0,
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	require.Len(t, gp.Edges, 1, "self-recursive call must produce a self-edge")
	e := gp.Edges[0]
	assert.Equal(t, "scip:go:"+sym, e.From)
	assert.Equal(t, "scip:go:"+sym, e.To)
	assert.Equal(t, contract.RelCalls, e.Relation)
}

// TestEmptyRelativePathSkipped verifies finding 11: documents with empty
// RelativePath are skipped and do not produce empty-string file entries.
func TestEmptyRelativePathSkipped(t *testing.T) {
	const sym = "go 1.0 `main`/Ghost()."
	idx := &scipbindings.Index{
		Documents: []*scipbindings.Document{
			{
				RelativePath: "", // empty -> must be skipped
				Language:     "go",
				Symbols: []*scipbindings.SymbolInformation{
					{Symbol: sym, Kind: scipbindings.SymbolInformation_Function, DisplayName: "Ghost"},
				},
				Occurrences: []*scipbindings.Occurrence{
					{
						Range:          []int32{0, 5, 0, 10},
						EnclosingRange: []int32{0, 0, 5, 0},
						Symbol:         sym,
						SymbolRoles:    int32(scipbindings.SymbolRole_Definition),
					},
				},
			},
		},
	}

	gp, err := scip.Parse(writeIndex(t, idx), "repo")
	require.NoError(t, err)
	assert.Empty(t, gp.Files, "empty RelativePath must produce no Files entry")
	assert.Empty(t, gp.Entities, "empty RelativePath must produce no entities")

	for _, f := range gp.Files {
		assert.NotEmpty(t, f, "Files must never contain an empty string")
	}
}
