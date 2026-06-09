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
