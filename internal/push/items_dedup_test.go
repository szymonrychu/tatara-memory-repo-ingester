package push_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

// TestItemsFromChunksDedup verifies that two chunks producing the same
// idempotency key (a genuine duplicate, e.g. a file listed twice) collapse to a
// single last-wins item, so the bulk batch never carries a duplicate key that
// tatara-memory rejects with a 400. Defensive backstop behind the analyzer fix.
func TestItemsFromChunksDedup(t *testing.T) {
	chunks := []contract.Chunk{
		{EntityID: "tf:variable:mod:x", Type: "tf_variable", FilePath: "mod/a.tf",
			Header: "[tf_variable] x", Body: "# mod/a.tf\nvariable \"x\" { ... }"},
		{EntityID: "tf:variable:mod:x", Type: "tf_variable", FilePath: "mod/a.tf",
			Header: "[tf_variable] x", Body: "# mod/a.tf\nvariable \"x\" { ... }"},
		{EntityID: "tf:variable:mod:y", Type: "tf_variable", FilePath: "mod/a.tf",
			Header: "[tf_variable] y", Body: "# mod/a.tf\nvariable \"y\" { ... }"},
	}
	items := push.ItemsFromChunks("terraform", chunks)
	require.Len(t, items, 2, "duplicate-keyed chunks must collapse to one item")

	keys := map[string]bool{}
	for _, it := range items {
		require.False(t, keys[it.IdempotencyKey], "duplicate key survived: %q", it.IdempotencyKey)
		keys[it.IdempotencyKey] = true
	}
}
