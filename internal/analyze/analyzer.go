// Package analyze defines the language-neutral analyzer contract and registry.
// Adding a language means adding one file implementing Analyzer and registering
// it; nothing else in the ingester changes.
package analyze

import (
	"context"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// Result is what an Analyzer emits for its assigned file set.
type Result struct {
	Entities   []contract.Entity
	Edges      []contract.Edge
	Chunks     []contract.Chunk
	Symbols    []contract.SymbolRow
	Hyperedges []contract.Hyperedge
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
