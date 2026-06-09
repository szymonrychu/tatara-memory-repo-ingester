package semantic

// LoadedFile is one analyzed file with its content, ready to be chunked.
type LoadedFile struct {
	Path    string
	Content string
}

// FileChunk is a token/size-bounded group of files extracted together.
type FileChunk struct {
	Files []LoadedFile
}

// ChunkBudget bounds a single chunk: at most MaxFiles files and MaxBytes of
// content. A single file larger than MaxBytes still gets its own chunk.
type ChunkBudget struct {
	MaxFiles int
	MaxBytes int
}

// DefaultChunkBudget is ~8 files and ~48KB of content per chunk (a rough proxy
// for the gpt-4o-mini context budget; ~4 bytes/token).
func DefaultChunkBudget() ChunkBudget {
	return ChunkBudget{MaxFiles: 8, MaxBytes: 48 * 1024}
}

// Chunk groups files greedily, closing a chunk when adding the next file would
// exceed either bound. Order is preserved.
func Chunk(files []LoadedFile, b ChunkBudget) []FileChunk {
	if b.MaxFiles <= 0 {
		b.MaxFiles = 8
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = 48 * 1024
	}
	var chunks []FileChunk
	var cur []LoadedFile
	curBytes := 0
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, FileChunk{Files: cur})
			cur = nil
			curBytes = 0
		}
	}
	for _, f := range files {
		n := len(f.Content)
		if len(cur) > 0 && (len(cur) >= b.MaxFiles || curBytes+n > b.MaxBytes) {
			flush()
		}
		cur = append(cur, f)
		curBytes += n
	}
	flush()
	return chunks
}
