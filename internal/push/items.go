package push

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// ItemsFromChunks turns analyzer chunks into /memories:bulk items. Items are
// deduplicated by idempotency key (last-wins): tatara-memory rejects a batch
// carrying a duplicate key with a 400, so any genuine duplicate (e.g. a file
// listed twice) must collapse to a single item rather than poison the batch.
func ItemsFromChunks(repo string, chunks []contract.Chunk) []contract.IngestItem {
	items := make([]contract.IngestItem, 0, len(chunks))
	idx := make(map[string]int, len(chunks))
	for _, ch := range chunks {
		text := ch.Header + "\n---\n" + ch.Body
		sum := sha256.Sum256([]byte(text))
		item := contract.IngestItem{
			IdempotencyKey: fmt.Sprintf("%s:%s:%s", repo, ch.EntityID, hex.EncodeToString(sum[:])),
			Text:           text,
			Metadata: map[string]string{
				"repo": repo, "entity_id": ch.EntityID, "type": ch.Type,
				"file_path": ch.FilePath, "language": ch.Language,
			},
		}
		if i, ok := idx[item.IdempotencyKey]; ok {
			items[i] = item
			continue
		}
		idx[item.IdempotencyKey] = len(items)
		items = append(items, item)
	}
	return items
}
