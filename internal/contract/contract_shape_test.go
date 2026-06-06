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
