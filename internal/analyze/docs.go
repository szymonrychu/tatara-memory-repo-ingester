package analyze

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type docsAnalyzer struct{}

// NewDocs returns the docs (chunk-only) analyzer.
func NewDocs() Analyzer { return docsAnalyzer{} }

func (docsAnalyzer) Name() string { return "docs" }

func (docsAnalyzer) Match(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt", ".rst":
		return true
	}
	return false
}

func (docsAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	var res Result
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(repoRoot, f)) //nolint:gosec
		if err != nil {
			continue // unreadable doc: skip, do not fail the run
		}
		lang := "markdown"
		if strings.ToLower(filepath.Ext(f)) == ".txt" {
			lang = "text"
		}
		res.Entities = append(res.Entities, contract.Entity{
			ID:       "doc:file:" + f,
			Name:     f,
			Type:     contract.EntityDocFile,
			FilePath: f,
		})
		res.Chunks = append(res.Chunks, contract.Chunk{
			EntityID: "doc:file:" + f,
			Type:     contract.EntityDocFile,
			FilePath: f,
			Language: lang,
			Header:   "[doc] " + f,
			Body:     string(b),
		})
	}
	return res, nil
}
