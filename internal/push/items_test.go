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

// TestIdempotencyKeyUsesFullSHA256 verifies that the idempotency key uses
// the full 32-byte (64 hex char) SHA-256 digest, not a truncated 8-byte
// prefix (finding 4: truncation risks collisions).
func TestIdempotencyKeyUsesFullSHA256(t *testing.T) {
	chunks := []contract.Chunk{{
		EntityID: "go:func:m.F", Type: contract.EntityGoFunc, FilePath: "m.go",
		Language: "go", Header: "[go_func] m.F", Body: "func F() {}",
	}}
	items := push.ItemsFromChunks("tatara-cli", chunks)
	require.Len(t, items, 1)
	key := items[0].IdempotencyKey
	// Key format: "<repo>:<entityID>:<hex-sha256>"
	// Split on ":" — the hex part is the last segment.
	parts := strings.Split(key, ":")
	require.True(t, len(parts) >= 3, "key must have at least 3 colon-separated parts")
	hexPart := parts[len(parts)-1]
	// Full SHA-256 hex is 64 characters; 8-byte truncation gives only 16.
	require.Equal(t, 64, len(hexPart), "idempotency key must use full 32-byte SHA-256 (64 hex chars), got %d chars", len(hexPart))
}
