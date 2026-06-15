package analyze

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type docsAnalyzer struct {
	log *slog.Logger
}

// NewDocs returns the docs analyzer. It emits one doc_file entity per file
// (so docs participate in the code graph) plus a semantic chunk, and captures
// YAML frontmatter (source_url/author/captured_at) onto the entity.
func NewDocs() Analyzer { return docsAnalyzer{log: slog.Default()} }

func (docsAnalyzer) Name() string { return "docs" }

func (docsAnalyzer) Match(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt", ".rst":
		return true
	}
	return false
}

func (d docsAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	var res Result
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(repoRoot, f)) //nolint:gosec
		if err != nil {
			d.log.Warn("docs: unreadable file; skipping", "file", f, "error", err)
			continue
		}
		lang := "markdown"
		if strings.ToLower(filepath.Ext(f)) == ".txt" {
			lang = "text"
		}
		fm, body := splitFrontmatter(string(b))
		ent := contract.Entity{
			ID:         "doc:file:" + f,
			Name:       f,
			Type:       contract.EntityDocFile,
			FilePath:   f,
			SourceURL:  fm.SourceURL,
			Author:     fm.Author,
			CapturedAt: fm.CapturedAt,
		}
		res.Entities = append(res.Entities, ent)
		res.Chunks = append(res.Chunks, contract.Chunk{
			EntityID: ent.ID,
			Type:     contract.EntityDocFile,
			FilePath: f,
			Language: lang,
			Header:   "[doc] " + f,
			Body:     body,
		})
	}
	return res, nil
}

// docFrontmatter is the subset of YAML frontmatter promoted to provenance columns.
type docFrontmatter struct {
	SourceURL  string `json:"source_url"`
	Author     string `json:"author"`
	CapturedAt string `json:"captured_at"`
}

// splitFrontmatter extracts a leading `---\n...\n---\n` YAML block and returns
// the parsed provenance plus the remaining body. With no frontmatter, it returns
// a zero docFrontmatter and the original content unchanged. A malformed block is
// ignored (zero provenance) and the original content is returned.
func splitFrontmatter(content string) (docFrontmatter, string) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return docFrontmatter{}, content
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return docFrontmatter{}, content
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---\n"):]
	var fm docFrontmatter
	if err := sigsyaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return docFrontmatter{}, content
	}
	return fm, body
}
