package push_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/push"
)

func TestItemsFromChunks(t *testing.T) {
	chunks := []contract.Chunk{{
		EntityID: "go:func:m.F", Type: contract.EntityGoFunc, FilePath: "m.go",
		Language: "go", Header: "[go_func] m.F", Body: "func F() {}",
	}}
	items := push.ItemsFromChunks("tatara-cli", chunks)
	require.Len(t, items, 1)
	require.Equal(t, "[go_func] m.F\n---\nfunc F() {}", items[0].Text)
	require.Equal(t, "go:func:m.F", items[0].Metadata["entity_id"])
	require.Equal(t, "tatara-cli", items[0].Metadata["repo"])
	require.True(t, strings.HasPrefix(items[0].IdempotencyKey, "tatara-cli:go:func:m.F:"))
}
