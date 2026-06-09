package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChunkGroupsByFileCount(t *testing.T) {
	files := make([]LoadedFile, 0, 20)
	for i := 0; i < 20; i++ {
		files = append(files, LoadedFile{Path: "f" + string(rune('a'+i)) + ".go", Content: "tiny"})
	}
	chunks := Chunk(files, ChunkBudget{MaxFiles: 8, MaxBytes: 1 << 20})
	require.Len(t, chunks, 3) // 8 + 8 + 4
	require.Len(t, chunks[0].Files, 8)
	require.Len(t, chunks[1].Files, 8)
	require.Len(t, chunks[2].Files, 4)
}

func TestChunkSplitsOnByteBudget(t *testing.T) {
	big := make([]byte, 600)
	for i := range big {
		big[i] = 'x'
	}
	files := []LoadedFile{
		{Path: "a.go", Content: string(big)},
		{Path: "b.go", Content: string(big)},
		{Path: "c.go", Content: string(big)},
	}
	// MaxBytes 1000 admits one 600-byte file per chunk (a second would exceed it).
	chunks := Chunk(files, ChunkBudget{MaxFiles: 8, MaxBytes: 1000})
	require.Len(t, chunks, 3)
	for _, c := range chunks {
		require.Len(t, c.Files, 1)
	}
}

func TestChunkOversizeFileGetsItsOwnChunk(t *testing.T) {
	big := make([]byte, 2000)
	files := []LoadedFile{
		{Path: "small.go", Content: "x"},
		{Path: "huge.go", Content: string(big)}, // exceeds MaxBytes alone
	}
	chunks := Chunk(files, ChunkBudget{MaxFiles: 8, MaxBytes: 1000})
	require.Len(t, chunks, 2)
	require.Equal(t, "small.go", chunks[0].Files[0].Path)
	require.Equal(t, "huge.go", chunks[1].Files[0].Path)
}

func TestChunkEmptyInputYieldsNoChunks(t *testing.T) {
	require.Empty(t, Chunk(nil, ChunkBudget{MaxFiles: 8, MaxBytes: 1000}))
}

func TestDefaultChunkBudget(t *testing.T) {
	b := DefaultChunkBudget()
	require.Equal(t, 8, b.MaxFiles)
	require.Greater(t, b.MaxBytes, 0)
}
