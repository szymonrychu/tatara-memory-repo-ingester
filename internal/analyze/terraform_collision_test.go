package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

// TestTerraformModuleScopedIDs verifies that same-named blocks in different root
// modules (directories) get distinct, module-dir-scoped entity IDs, distinct
// chunk content, and distinct bulk idempotency keys - so the whole-repo bulk push
// never carries two items with the same key (the terraform-ingest 400 incident).
func TestTerraformModuleScopedIDs(t *testing.T) {
	a := analyze.NewTerraform()
	res, err := a.Analyze(context.Background(), "testdata/tf",
		[]string{"mod_a/main.tf", "mod_b/main.tf"})
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}
	// Module-dir-scoped IDs, distinct across the two identical modules.
	for _, want := range []string{
		"tf:variable:mod_a:shared", "tf:variable:mod_b:shared",
		"tf:resource:mod_a:null_resource.r", "tf:resource:mod_b:null_resource.r",
		"tf:output:mod_a:o", "tf:output:mod_b:o",
	} {
		require.True(t, ids[want], "missing module-scoped entity %q", want)
	}

	// Edges resolve WITHIN the referencing block's module dir.
	_, ok := findEdge(res.Edges, contract.RelVarRef,
		"tf:resource:mod_a:null_resource.r", "tf:variable:mod_a:shared")
	require.True(t, ok, "var_ref must resolve to the same-module variable (mod_a)")
	_, ok = findEdge(res.Edges, contract.RelReferences,
		"tf:output:mod_b:o", "tf:resource:mod_b:null_resource.r")
	require.True(t, ok, "references must resolve to the same-module resource (mod_b)")
	// No cross-module leakage.
	_, ok = findEdge(res.Edges, contract.RelVarRef,
		"tf:resource:mod_a:null_resource.r", "tf:variable:mod_b:shared")
	require.False(t, ok, "var_ref must not point at another module's variable")

	// Chunk bodies for the two identical modules must differ (else LightRAG
	// content-dedup silently collapses them to one memory).
	bodyA, bodyB := "", ""
	for _, c := range res.Chunks {
		if c.EntityID == "tf:variable:mod_a:shared" {
			bodyA = c.Body
		}
		if c.EntityID == "tf:variable:mod_b:shared" {
			bodyB = c.Body
		}
	}
	require.NotEmpty(t, bodyA)
	require.NotEmpty(t, bodyB)
	require.NotEqual(t, bodyA, bodyB, "identical-content cross-module chunks must have distinct bodies")

	// The whole-repo bulk batch must have no duplicate idempotency keys.
	items := push.ItemsFromChunks("terraform", res.Chunks)
	seen := map[string]bool{}
	for _, it := range items {
		require.False(t, seen[it.IdempotencyKey],
			"duplicate idempotency key in batch: %q", it.IdempotencyKey)
		seen[it.IdempotencyKey] = true
	}
}
