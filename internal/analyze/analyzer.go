// Package analyze defines the language-neutral analyzer contract and registry.
// Adding a language means adding one file implementing Analyzer and registering
// it; nothing else in the ingester changes.
package analyze

import (
	"context"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// skipWalkDirs are directory names that must never be descended into when a
// language analyzer walks the whole repo to build its resolution index. They
// hold dependencies, VCS metadata, or build output - parsing them is wasteful
// and pollutes the name->entityID index with symbols that are never emitted as
// entities (entities are only emitted for diff-set files), which would resolve
// imports to phantom targets. filepath.WalkDir does not honour .gitignore, so
// this prune list is the only thing keeping node_modules/vendor/.git out.
var skipWalkDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".tox":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
}

// shouldSkipWalkDir reports whether a directory named dirName should be pruned
// from a repo-wide analyzer walk (it appears in skipWalkDirs).
func shouldSkipWalkDir(dirName string) bool {
	return skipWalkDirs[dirName]
}

// Result is what an Analyzer emits for its assigned file set.
type Result struct {
	Entities   []contract.Entity
	Edges      []contract.Edge
	Chunks     []contract.Chunk
	Symbols    []contract.SymbolRow
	Hyperedges []contract.Hyperedge
	// ParseErrors counts per-file parse errors (tree-sitter / HCL / helm template)
	// that were WARNed and skipped inside the analyzer. Callers should accumulate
	// this value and feed it to the AnalyzerParseErrorsTotal metric.
	ParseErrors int
}

// Analyzer extracts a code graph and chunks for one language/file class.
type Analyzer interface {
	Name() string
	Match(path string) bool
	Analyze(ctx context.Context, repoRoot string, files []string) (Result, error)
}

// Registry is an ordered set of analyzers; earlier registration wins on Match.
type Registry struct {
	analyzers []Analyzer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register appends an analyzer (precedence = registration order).
func (r *Registry) Register(a Analyzer) { r.analyzers = append(r.analyzers, a) }

// Analyzers returns the registered analyzers in order.
func (r *Registry) Analyzers() []Analyzer { return r.analyzers }

// Group assigns each file to the first analyzer whose Match returns true.
// Files matched by no analyzer are dropped.
func (r *Registry) Group(files []string) map[string][]string {
	groups := map[string][]string{}
	for _, f := range files {
		for _, a := range r.analyzers {
			if a.Match(f) {
				groups[a.Name()] = append(groups[a.Name()], f)
				break
			}
		}
	}
	return groups
}
