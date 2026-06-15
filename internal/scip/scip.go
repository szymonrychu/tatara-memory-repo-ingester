// Package scip parses a SCIP index protobuf and maps it to a contract.GraphPush.
package scip

import (
	"fmt"
	"os"
	"sort"
	"strings"

	scipbindings "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// Parse reads the SCIP index at path and maps it to a GraphPush for repo.
// The path is a user-supplied CLI flag, gosec file-inclusion warning is expected.
//
//nolint:gosec
func Parse(path, repo string) (contract.GraphPush, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return contract.GraphPush{}, fmt.Errorf("scip: read %s: %w", path, err)
	}

	var idx scipbindings.Index
	if err := proto.Unmarshal(raw, &idx); err != nil {
		return contract.GraphPush{}, fmt.Errorf("scip: unmarshal: %w", err)
	}

	var (
		entities []contract.Entity
		edges    []contract.Edge
		symbols  []contract.SymbolRow
		fileSet  = make(map[string]struct{})
	)

	// Build a top-level symInfo from Index.ExternalSymbols so that external
	// (imported/cross-package) symbol kinds are available when classifying
	// reference edges. Per-document Symbols are overlaid on top below.
	globalSymInfo := make(map[string]*scipbindings.SymbolInformation, len(idx.ExternalSymbols))
	for _, si := range idx.ExternalSymbols {
		if si != nil {
			globalSymInfo[si.Symbol] = si
		}
	}

	for _, doc := range idx.Documents {
		if doc == nil {
			continue
		}
		// Finding 11: skip documents with empty RelativePath; an empty path would
		// produce unscoped entities/files on the server reconcile.
		if doc.RelativePath == "" {
			continue
		}
		fileSet[doc.RelativePath] = struct{}{}
		lang := strings.ToLower(doc.Language)

		// Index symbol metadata by symbol string; overlay doc-local symbols over
		// the global (ExternalSymbols) map so local definitions win.
		symInfo := make(map[string]*scipbindings.SymbolInformation, len(globalSymInfo)+len(doc.Symbols))
		for k, v := range globalSymInfo {
			symInfo[k] = v
		}
		for _, si := range doc.Symbols {
			if si != nil {
				symInfo[si.Symbol] = si
			}
		}

		// Separate definition occurrences from reference occurrences.
		var defs []*scipbindings.Occurrence
		var refs []*scipbindings.Occurrence
		for _, occ := range doc.Occurrences {
			if occ == nil {
				continue
			}
			if occ.SymbolRoles&int32(scipbindings.SymbolRole_Definition) != 0 {
				defs = append(defs, occ)
			} else {
				refs = append(refs, occ)
			}
		}

		// Emit one entity per definition.
		defBySymbol := make(map[string]*scipbindings.Occurrence, len(defs))
		for _, d := range defs {
			defBySymbol[d.Symbol] = d
			// Prefer EnclosingRange for full body span; fall back to Range.
			lineStart, lineEnd := occLineStartEnd(bestRange(d))
			si, ok := symInfo[d.Symbol]
			if !ok {
				// No SymbolInformation: emit a minimal entity.
				entities = append(entities, contract.Entity{
					ID:        entityID(lang, d.Symbol),
					Name:      lastComponent(d.Symbol),
					Type:      "scip_symbol",
					FilePath:  doc.RelativePath,
					LineStart: lineStart,
					LineEnd:   lineEnd,
					Properties: map[string]string{
						"scip_kind": "unspecified",
					},
				})
				// Emit provides SymbolRow for cross-repo resolution.
				symbols = append(symbols, contract.SymbolRow{
					Symbol:   d.Symbol,
					Lang:     lang,
					Kind:     "scip_symbol",
					Role:     contract.RoleProvides,
					EntityID: entityID(lang, d.Symbol),
					SrcFile:  doc.RelativePath,
				})
				continue
			}
			kind := si.Kind
			entities = append(entities, contract.Entity{
				ID:        entityID(lang, si.Symbol),
				Name:      displayName(si),
				Type:      scipKind(kind),
				FilePath:  doc.RelativePath,
				LineStart: lineStart,
				LineEnd:   lineEnd,
				Properties: map[string]string{
					"scip_kind": kind.String(),
				},
			})
			// Emit provides SymbolRow for cross-repo resolution.
			symbols = append(symbols, contract.SymbolRow{
				Symbol:   si.Symbol,
				Lang:     lang,
				Kind:     scipKind(kind),
				Role:     contract.RoleProvides,
				EntityID: entityID(lang, si.Symbol),
				SrcFile:  doc.RelativePath,
			})
		}

		// Track which symbols have a local definition so we can emit requires rows
		// for references that resolve only via ExternalSymbols.
		localDefSet := make(map[string]struct{}, len(defs))
		for _, d := range defs {
			localDefSet[d.Symbol] = struct{}{}
		}

		// Emit edges: each reference occurrence -> find enclosing def -> emit edge.
		// Deduplicate by (from, to, relation) to avoid redundant pushes.
		type edgeKey struct{ from, to, rel string }
		seenEdges := make(map[edgeKey]struct{})
		// Deduplicate requires SymbolRows too.
		seenRequires := make(map[string]struct{})

		for _, ref := range refs {
			if ref.Symbol == "" {
				continue
			}
			enclosing, ok := enclosingDef(defs, ref)
			if !ok {
				continue
			}
			// Determine relation from the referenced symbol's kind.
			rel := contract.RelReferences
			if si, ok2 := symInfo[ref.Symbol]; ok2 {
				if isFunctionLike(si.Kind) {
					rel = contract.RelCalls
				}
			}

			ek := edgeKey{from: entityID(lang, enclosing.Symbol), to: entityID(lang, ref.Symbol), rel: rel}
			if _, seen := seenEdges[ek]; seen {
				continue
			}
			seenEdges[ek] = struct{}{}

			const confScore = 0.98
			edges = append(edges, contract.Edge{
				From:            ek.from,
				To:              ek.to,
				Relation:        ek.rel,
				SrcFile:         doc.RelativePath,
				ConfidenceScore: confScore,
				ConfidenceTier:  contract.TierForScore(confScore),
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})

			// Emit requires SymbolRow when the referenced symbol has no local def
			// (i.e. it comes from ExternalSymbols or is absent).
			if _, isLocal := localDefSet[ref.Symbol]; !isLocal {
				if _, alreadySeen := seenRequires[ref.Symbol]; !alreadySeen {
					seenRequires[ref.Symbol] = struct{}{}
					reqKind := "scip_symbol"
					if si, ok2 := symInfo[ref.Symbol]; ok2 {
						reqKind = scipKind(si.Kind)
					}
					symbols = append(symbols, contract.SymbolRow{
						Symbol:   ref.Symbol,
						Lang:     lang,
						Kind:     reqKind,
						Role:     contract.RoleRequires,
						EntityID: entityID(lang, ref.Symbol),
						SrcFile:  doc.RelativePath,
					})
				}
			}
		}
	}

	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	return contract.GraphPush{
		Repo:     repo,
		Files:    files,
		Entities: entities,
		Edges:    edges,
		Symbols:  symbols,
	}, nil
}

func entityID(lang, symbol string) string {
	return "scip:" + lang + ":" + symbol
}

func displayName(si *scipbindings.SymbolInformation) string {
	if si.DisplayName != "" {
		return si.DisplayName
	}
	return lastComponent(si.Symbol)
}

// lastComponent returns the last meaningful name component of a SCIP symbol
// string by delegating to the canonical scip symbol parser. This handles
// descriptor suffix punctuation correctly (e.g. `T#m().` -> "m").
// Falls back to simple string splitting when the symbol cannot be parsed
// (e.g. local symbols).
func lastComponent(sym string) string {
	parsed, err := scipbindings.ParseSymbol(sym)
	if err == nil && parsed != nil && len(parsed.Descriptors) > 0 {
		last := parsed.Descriptors[len(parsed.Descriptors)-1]
		if last.Name != "" {
			return last.Name
		}
	}
	// Fallback for local symbols or unparseable inputs.
	for _, sep := range []string{"/", ".", "`"} {
		if idx := strings.LastIndex(sym, sep); idx >= 0 && idx < len(sym)-1 {
			rest := sym[idx+1:]
			rest = strings.TrimRight(rest, ".()")
			if rest != "" {
				return rest
			}
		}
	}
	return sym
}

// scipKind maps a SCIP SymbolInformation_Kind to a contract entity type.
func scipKind(k scipbindings.SymbolInformation_Kind) string {
	switch k {
	case scipbindings.SymbolInformation_Function,
		scipbindings.SymbolInformation_Constructor:
		return "scip_function"
	case scipbindings.SymbolInformation_Method,
		scipbindings.SymbolInformation_MethodAlias,
		scipbindings.SymbolInformation_MethodReceiver,
		scipbindings.SymbolInformation_MethodSpecification,
		scipbindings.SymbolInformation_AbstractMethod:
		return "scip_method"
	case scipbindings.SymbolInformation_Class,
		scipbindings.SymbolInformation_Struct,
		scipbindings.SymbolInformation_Interface:
		return "scip_type"
	case scipbindings.SymbolInformation_Variable,
		scipbindings.SymbolInformation_Field:
		return "scip_variable"
	default:
		return "scip_symbol"
	}
}

// isFunctionLike returns true when kind represents a callable entity.
func isFunctionLike(k scipbindings.SymbolInformation_Kind) bool {
	switch k {
	case scipbindings.SymbolInformation_Function,
		scipbindings.SymbolInformation_Constructor,
		scipbindings.SymbolInformation_Method,
		scipbindings.SymbolInformation_MethodAlias,
		scipbindings.SymbolInformation_MethodReceiver,
		scipbindings.SymbolInformation_MethodSpecification,
		scipbindings.SymbolInformation_AbstractMethod:
		return true
	}
	return false
}

// bestRange returns EnclosingRange when it is non-empty (i.e. the body span
// as emitted by real SCIP indexers), otherwise falls back to Range (the
// name-token range).
func bestRange(occ *scipbindings.Occurrence) []int32 {
	if len(occ.EnclosingRange) >= 3 {
		return occ.EnclosingRange
	}
	return occ.Range
}

// enclosingDef finds the definition occurrence whose body contains the start
// of ref. It uses EnclosingRange (the function body span) for containment and
// falls back to Range only when EnclosingRange is absent.
//
// SCIP ranges are half-open [start, end): for 4-element ranges we use
// `refLine < endLine` (exclusive end) to avoid attributing a reference on the
// first line after the body to the preceding definition.
//
// Only the exact def name-token occurrence (same symbol AND same range) is
// skipped; a reference to the same symbol at a different range (recursion) is
// allowed to produce a self-edge.
func enclosingDef(defs []*scipbindings.Occurrence, ref *scipbindings.Occurrence) (*scipbindings.Occurrence, bool) {
	refLine, _ := occStartLine(ref.Range)
	for _, d := range defs {
		// Skip only the def's own name-token occurrence (same symbol + same range),
		// not legitimate recursive self-references.
		if d.Symbol == ref.Symbol && rangesEqual(d.Range, ref.Range) {
			continue
		}
		r := bestRange(d)
		startLine, ok1 := occStartLine(r)
		endLine, ok2 := occEndLineBestRange(r)
		if !ok1 || !ok2 {
			continue
		}
		if refLine >= startLine && refLine < endLine {
			return d, true
		}
	}
	return nil, false
}

// rangesEqual returns true when two occurrence ranges are identical.
func rangesEqual(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func occStartLine(r []int32) (int32, bool) {
	if len(r) < 2 {
		return 0, false
	}
	return r[0], true
}

// occEndLineBestRange returns the exclusive end line for use in containment
// checks. For 4-element ranges the end is exclusive per the SCIP spec's
// half-open [start, end) convention; for 3-element (same-line) ranges the
// start+1 is used so that a reference on the same line is included.
func occEndLineBestRange(r []int32) (int32, bool) {
	switch len(r) {
	case 3:
		// [startLine, startChar, endChar] - single line; use start+1 as exclusive end.
		return r[0] + 1, true
	case 4:
		// [startLine, startChar, endLine, endChar] - half-open, endLine is exclusive.
		return r[2], true
	default:
		return 0, false
	}
}

// occEndLine returns the (inclusive, for display) end line from a range.
// Used only for LineEnd on entities, not for containment logic.
func occEndLine(r []int32) (int32, bool) {
	switch len(r) {
	case 3:
		// single line
		return r[0], true
	case 4:
		return r[2], true
	default:
		return 0, false
	}
}

// occLineStartEnd returns 1-based LineStart and 0-based LineEnd from a SCIP
// occurrence Range. SCIP lines are 0-based; we convert start to 1-based to
// match the contract's convention (AST analyzers also emit 1-based LineStart).
// Returns (0, 0) when the range is malformed.
func occLineStartEnd(r []int32) (lineStart, lineEnd int) {
	start, ok1 := occStartLine(r)
	end, ok2 := occEndLine(r)
	if !ok1 || !ok2 {
		return 0, 0
	}
	return int(start) + 1, int(end)
}
