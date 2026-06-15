package analyze_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestDocsAnalyzerCapturesFrontmatter(t *testing.T) {
	a := analyze.NewDocs()
	res, err := a.Analyze(context.Background(), "testdata/docs", []string{"front.md"})
	require.NoError(t, err)
	require.Len(t, res.Entities, 1)
	e := res.Entities[0]
	require.Equal(t, "https://example.com/origin", e.SourceURL)
	require.Equal(t, "Alice Example", e.Author)
	require.Equal(t, "2026-06-09T12:00:00Z", e.CapturedAt)

	require.Len(t, res.Chunks, 1)
	require.Contains(t, res.Chunks[0].Body, "This document came from elsewhere.")
	require.NotContains(t, res.Chunks[0].Body, "source_url",
		"frontmatter block must be stripped from the chunk body")
}

func TestDocsAnalyzerNoFrontmatter(t *testing.T) {
	a := analyze.NewDocs()
	res, err := a.Analyze(context.Background(), "testdata/docs", []string{"README.md"})
	require.NoError(t, err)
	require.Len(t, res.Entities, 1)
	e := res.Entities[0]
	require.Empty(t, e.SourceURL)
	require.Empty(t, e.Author)
	require.Empty(t, e.CapturedAt)
	require.Contains(t, res.Chunks[0].Body, "Some prose.")
}

// TestDocsAnalyzerWarnsOnUnreadableFile verifies that an unreadable file path
// produces a WARN log entry (rule 12) and does not cause an error or panic.
// Before the fix, a missing file was silently skipped (bare continue, no log).
func TestDocsAnalyzerWarnsOnUnreadableFile(t *testing.T) {
	var logBuf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	a := analyze.NewDocs()
	// "no_such_file.md" does not exist in testdata/docs.
	res, err := a.Analyze(context.Background(), "testdata/docs", []string{"no_such_file.md"})
	require.NoError(t, err, "unreadable file must not fail the run")
	require.Empty(t, res.Entities, "unreadable file must not produce entities")

	assert.Contains(t, logBuf.String(), "no_such_file.md", "WARN log must mention the skipped path")
}

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
