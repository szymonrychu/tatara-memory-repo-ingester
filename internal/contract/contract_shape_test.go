package contract_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestGraphPushJSONShape(t *testing.T) {
	p := contract.GraphPush{
		Repo:   "tatara-cli",
		Commit: "abc123",
		Files:  []string{"cmd/root.go"},
		Entities: []contract.Entity{{
			ID: "go:func:m.F", Name: "F", Type: contract.EntityGoFunc,
			FilePath: "cmd/root.go", Properties: map[string]string{"resolution": contract.ResTypeResolved},
		}},
		Edges: []contract.Edge{{
			From: "go:func:m.F", To: "go:func:m.G", Relation: contract.RelCalls,
			SrcFile: "cmd/root.go", Properties: map[string]string{"confidence": "0.98"},
		}},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t, []string{"repo", "commit", "files", "entities", "edges"}, keys(got))
	ent := got["entities"].([]any)[0].(map[string]any)
	require.ElementsMatch(t, []string{"id", "name", "type", "file_path", "properties"}, keys(ent))
	edge := got["edges"].([]any)[0].(map[string]any)
	require.ElementsMatch(t, []string{"from", "to", "relation", "src_file", "properties"}, keys(edge))
}

func TestIngestItemJSONShape(t *testing.T) {
	it := contract.IngestItem{IdempotencyKey: "k", Text: "t", Metadata: map[string]string{"repo": "r"}}
	b, err := json.Marshal(it)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t, []string{"idempotency_key", "text", "metadata"}, keys(got))
}

func TestSymbolRowJSONShape(t *testing.T) {
	s := contract.SymbolRow{
		Symbol:   "github.com/szymonrychu/other.Func",
		Lang:     "go",
		Kind:     "func",
		Role:     contract.RoleProvides,
		EntityID: "go:func:github.com/szymonrychu/other.Func",
		SrcFile:  "pkg/pkg.go",
	}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t, []string{"symbol", "lang", "kind", "role", "entity_id", "src_file"}, keys(got))
}

func TestGraphPushSymbolsOmitEmpty(t *testing.T) {
	p := contract.GraphPush{
		Repo:     "tatara-cli",
		Files:    []string{"a.go"},
		Entities: []contract.Entity{},
		Edges:    []contract.Edge{},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, hasSymbols := got["symbols"]
	require.False(t, hasSymbols, "symbols key must be absent when empty")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBulkMemoriesRequestShape(t *testing.T) {
	req := contract.BulkMemoriesRequest{
		Repo:           "tatara-cli",
		ReconcileFiles: []string{"a.go", "b.md"},
		Items:          []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}},
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t, []string{"repo", "reconcile_files", "items"}, keys(got))
	require.Equal(t, "tatara-cli", got["repo"])
	require.Len(t, got["reconcile_files"].([]any), 2)
}

func TestBulkMemoriesRequestRepoOmitEmpty(t *testing.T) {
	req := contract.BulkMemoriesRequest{Items: []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}}}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, has := got["repo"]
	require.False(t, has, "repo must be absent when empty")
}

func TestBulkMemoriesRequestReconcileOmitEmpty(t *testing.T) {
	req := contract.BulkMemoriesRequest{Items: []contract.IngestItem{{IdempotencyKey: "k", Text: "t"}}}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, has := got["reconcile_files"]
	require.False(t, has, "reconcile_files must be absent when empty")
	require.ElementsMatch(t, []string{"items"}, keys(got))
}

func TestHyperedgeJSONShape(t *testing.T) {
	h := contract.Hyperedge{
		ID: "he:1", Label: "trait impl", Relation: "implement",
		ConfidenceScore: 0.9, SrcFile: "x.go",
		Members:    []string{"go:type:m.A", "go:type:m.B", "go:type:m.C"},
		Properties: map[string]string{"k": "v"},
	}
	b, err := json.Marshal(h)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t,
		[]string{"id", "label", "relation", "confidence_score", "src_file", "members", "properties"},
		keys(got))
	require.Len(t, got["members"].([]any), 3)
}

func TestGraphPushHyperedgesOmitEmpty(t *testing.T) {
	p := contract.GraphPush{Repo: "r", Files: []string{"a.go"}, Entities: []contract.Entity{}, Edges: []contract.Edge{}}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, has := got["hyperedges"]
	require.False(t, has, "hyperedges key must be absent when empty")
}

func TestGraphPushHyperedgesPresentWhenSet(t *testing.T) {
	p := contract.GraphPush{
		Repo: "r", Files: []string{"a.go"}, Entities: []contract.Entity{}, Edges: []contract.Edge{},
		Hyperedges: []contract.Hyperedge{{ID: "he:1", Label: "l", Relation: "form", SrcFile: "a.go",
			Members: []string{"x", "y", "z"}}},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got["hyperedges"].([]any), 1)
}

func TestSemanticRelationConstants(t *testing.T) {
	require.Equal(t, "conceptually_related_to", contract.RelConceptuallyRelated)
	require.Equal(t, "semantically_similar_to", contract.RelSemanticallySimilar)
	require.Equal(t, "rationale_for", contract.RelRationaleFor)
	require.Equal(t, "shares_data_with", contract.RelSharesDataWith)
	require.Equal(t, "cites", contract.RelCites)
}

func TestEntityProvenanceFields(t *testing.T) {
	e := contract.Entity{
		ID: "doc:file:README.md", Name: "README.md", Type: contract.EntityDocFile,
		FilePath: "README.md", LineStart: 1, LineEnd: 42,
		SourceURL: "https://example.com/x", Author: "alice", CapturedAt: "2026-06-09T00:00:00Z",
	}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t,
		[]string{"id", "name", "type", "file_path", "line_start", "line_end", "source_url", "author", "captured_at"},
		keys(got))
}

func TestEntityProvenanceOmitEmpty(t *testing.T) {
	e := contract.Entity{ID: "go:package:m", Name: "m", Type: contract.EntityGoPackage, FilePath: ""}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	for _, k := range []string{"line_start", "line_end", "source_url", "author", "captured_at"} {
		_, has := got[k]
		require.False(t, has, "%s must be absent when empty", k)
	}
}

func TestDocEntityTypeConstants(t *testing.T) {
	require.Equal(t, "doc_file", contract.EntityDocFile)
	require.Equal(t, "doc_section", contract.EntityDocSection)
	require.Equal(t, "concept", contract.EntityConcept)
	require.Equal(t, "rationale", contract.EntityRationale)
}

func TestEdgeConfidenceFields(t *testing.T) {
	e := contract.Edge{
		From: "a", To: "b", Relation: contract.RelCalls, SrcFile: "x.go",
		ConfidenceScore: 0.98, ConfidenceTier: contract.TierInferred,
		Properties: map[string]string{"resolution": contract.ResTypeResolved},
	}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t,
		[]string{"from", "to", "relation", "src_file", "confidence_score", "confidence_tier", "properties"},
		keys(got))
	require.InDelta(t, 0.98, got["confidence_score"], 1e-9)
	require.Equal(t, "INFERRED", got["confidence_tier"])
}

func TestEdgeConfidenceOmitEmpty(t *testing.T) {
	e := contract.Edge{From: "a", To: "b", Relation: contract.RelCalls, SrcFile: "x.go"}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, hasScore := got["confidence_score"]
	_, hasTier := got["confidence_tier"]
	require.False(t, hasScore, "confidence_score must be absent when zero")
	require.False(t, hasTier, "confidence_tier must be absent when empty")
}

func TestTierForScore(t *testing.T) {
	cases := []struct {
		name  string
		score float64
		tier  string
	}{
		{"extracted", 1.0, contract.TierExtracted},
		{"inferred_high", 0.98, contract.TierInferred},
		{"inferred_mid", 0.45, contract.TierInferred},
		{"ambiguous_boundary", 0.3, contract.TierAmbiguous},
		{"ambiguous_low", 0.0, contract.TierAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.tier, contract.TierForScore(tc.score))
		})
	}
}

// TestExtractorConstants guards the string values of all three recognised
// extractor origin tags against silent renaming. If ExtractorSCIP drifts from
// "scip" the server will silently create a fourth, orphaned extractor bucket.
func TestExtractorConstants(t *testing.T) {
	require.Equal(t, "ast", contract.ExtractorAST)
	require.Equal(t, "semantic", contract.ExtractorSemantic)
	require.Equal(t, "scip", contract.ExtractorSCIP)
}

func TestGraphPushSCIPExtractorShape(t *testing.T) {
	p := contract.GraphPush{
		Repo:      "tatara-cli",
		Commit:    "abc123",
		Extractor: contract.ExtractorSCIP,
		Files:     []string{"cmd/root.go"},
		Entities:  []contract.Entity{},
		Edges:     []contract.Edge{},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, "scip", got["extractor"], "ExtractorSCIP must serialise as 'scip'")
}

func TestGraphPushExtractorAndFileSHAsShape(t *testing.T) {
	p := contract.GraphPush{
		Repo:      "tatara-cli",
		Commit:    "abc123",
		Extractor: "semantic",
		Files:     []string{"cmd/root.go"},
		Entities:  []contract.Entity{},
		Edges:     []contract.Edge{},
		FileSHAs:  map[string]string{"cmd/root.go": "deadbeef"},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t,
		[]string{"repo", "commit", "extractor", "files", "entities", "edges", "file_shas"},
		keys(got))
	require.Equal(t, "semantic", got["extractor"])
	shas := got["file_shas"].(map[string]any)
	require.Equal(t, "deadbeef", shas["cmd/root.go"])
}

func TestGraphPushExtractorAndFileSHAsOmitEmpty(t *testing.T) {
	p := contract.GraphPush{Repo: "r", Files: []string{"a.go"}, Entities: []contract.Entity{}, Edges: []contract.Edge{}}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	_, hasExtractor := got["extractor"]
	_, hasSHAs := got["file_shas"]
	require.False(t, hasExtractor, "extractor must be absent when empty")
	require.False(t, hasSHAs, "file_shas must be absent when empty")
}

func TestSemanticMissesRequestShape(t *testing.T) {
	req := contract.SemanticMissesRequest{
		Repo: "r",
		Files: []contract.FileSHA{
			{Path: "a.go", ContentSHA: "sha1"},
			{Path: "b.go", ContentSHA: "sha2"},
		},
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.ElementsMatch(t, []string{"repo", "files"}, keys(got))
	first := got["files"].([]any)[0].(map[string]any)
	require.ElementsMatch(t, []string{"path", "content_sha"}, keys(first))
}
