package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestDocsAnalyzer(t *testing.T) {
	a := analyze.NewDocs()
	require.True(t, a.Match("README.md"))
	require.True(t, a.Match("docs/guide.txt"))
	require.False(t, a.Match("main.go"))

	res, err := a.Analyze(context.Background(), "testdata/docs", []string{"README.md"})
	require.NoError(t, err)
	require.Len(t, res.Chunks, 1)
	require.Equal(t, "markdown", res.Chunks[0].Language)
	require.Contains(t, res.Chunks[0].Body, "Some prose.")

	require.Len(t, res.Entities, 1, "doc files now emit a doc entity")
	e := res.Entities[0]
	require.Equal(t, contract.EntityDocFile, e.Type)
	require.Equal(t, "doc:file:README.md", e.ID)
	require.Equal(t, "README.md", e.FilePath)
	require.Equal(t, "README.md", e.Name)
	require.Empty(t, res.Edges)
}
