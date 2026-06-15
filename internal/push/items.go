package push

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// ItemsFromChunks turns analyzer chunks into /memories:bulk items.
func ItemsFromChunks(repo string, chunks []contract.Chunk) []contract.IngestItem {
	items := make([]contract.IngestItem, 0, len(chunks))
	for _, ch := range chunks {
		text := ch.Header + "\n---\n" + ch.Body
		sum := sha256.Sum256([]byte(text))
		items = append(items, contract.IngestItem{
			IdempotencyKey: fmt.Sprintf("%s:%s:%s", repo, ch.EntityID, hex.EncodeToString(sum[:])),
			Text:           text,
			Metadata: map[string]string{
				"repo": repo, "entity_id": ch.EntityID, "type": ch.Type,
				"file_path": ch.FilePath, "language": ch.Language,
			},
		})
	}
	return items
}
