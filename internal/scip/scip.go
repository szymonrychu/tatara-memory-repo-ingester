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

	// realEntityIDs holds every entity ID emitted from a Definition occurrence,
	// across ALL documents. A symbol may be referenced in one document and defined
	// in another; without a global set the per-document placeholder logic would
	// emit a scip_external placeholder AND the real entity for the same ID,
	// producing duplicate rows that clobber the real entity on reconcile.
	realEntityIDs := make(map[string]struct{})
	// placeholderCandidates collects external edge targets that had no local
	// definition in their own document. Keyed by entity ID so cross-document
	// duplicates collapse. Emitted after the loop, but only for IDs that never
	// got a real entity anywhere in the index.
	placeholderCandidates := make(map[string]contract.Entity)

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

		// seenEntityIDs deduplicates entity rows within this document; a symbol
		// may appear as a Definition occurrence more than once (e.g. forward
		// declaration + definition, or an indexer that re-emits the same def).
		seenEntityIDs := make(map[string]struct{}, len(defs))

		// Emit one entity per definition, deduplicating by entity ID.
		for _, d := range defs {
			eid := entityID(lang, d.Symbol)
			if _, seen := seenEntityIDs[eid]; seen {
				continue
			}
			seenEntityIDs[eid] = struct{}{}
			realEntityIDs[eid] = struct{}{}

			// Prefer EnclosingRange for full body span; fall back to Range.
			lineStart, lineEnd := occLineStartEnd(bestRange(d))
			si, ok := symInfo[d.Symbol]
			if !ok {
				// No SymbolInformation: emit a minimal entity.
				entities = append(entities, contract.Entity{
					ID:        eid,
					Name:      lastComponent(d.Symbol),
					Type:      "scip_symbol",
					FilePath:  doc.RelativePath,
					LineStart: lineStart,
					LineEnd:   lineEnd,
					Properties: map[string]string{
						"scip_kind": "unspecified",
					},
				})
				// SymbolRows (provides/requires) for cross-repo resolution are deferred to
				// ROADMAP: raw SCIP moniker strings do not match AST canonical symbols and
				// the reconcile delete is not extractor-scoped, so emitting them would
				// clobber AST rows. See MEMORY.md 2026-06-06 + findings 1, 2, 6.
				continue
			}
			kind := si.Kind
			entities = append(entities, contract.Entity{
				ID:        eid,
				Name:      displayName(si),
				Type:      scipKind(kind),
				FilePath:  doc.RelativePath,
				LineStart: lineStart,
				LineEnd:   lineEnd,
				Properties: map[string]string{
					"scip_kind": kind.String(),
				},
			})
		}

		// Emit edges: each reference occurrence -> find enclosing def -> emit edge.
		// Deduplicate by (from, to, relation) to avoid redundant pushes.
		type edgeKey struct{ from, to, rel string }
		seenEdges := make(map[edgeKey]struct{})

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

			// If the To entity has no local entity in this document, record a
			// placeholder candidate so the edge target resolves and the graph has
			// no dangling edges. Emission is deferred to a post-loop pass that drops
			// any candidate whose ID later (or in another document) got a real
			// entity, preventing duplicate rows for the same ID.
			if _, exists := seenEntityIDs[ek.to]; !exists {
				if _, queued := placeholderCandidates[ek.to]; !queued {
					name := lastComponent(ref.Symbol)
					if extSI, extOK := symInfo[ref.Symbol]; extOK && extSI.DisplayName != "" {
						name = extSI.DisplayName
					}
					placeholderCandidates[ek.to] = contract.Entity{
						ID:   ek.to,
						Name: name,
						Type: "scip_external",
						Properties: map[string]string{
							"scip_kind": "external",
						},
					}
				}
			}

			// SCIP type resolution is compiler-grade: use score=1.0 so edges rank
			// as EXTRACTED, matching the certainty of AST-extracted facts.
			const confScore = 1.0
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
		}
	}

	// Emit placeholder entities for external edge targets, but only for IDs that
	// never got a real entity anywhere in the index. Sorted for deterministic
	// output (the candidate map iteration order is otherwise random).
	placeholderIDs := make([]string, 0, len(placeholderCandidates))
	for id := range placeholderCandidates {
		if _, real := realEntityIDs[id]; real {
			continue
		}
		placeholderIDs = append(placeholderIDs, id)
	}
	sort.Strings(placeholderIDs)
	for _, id := range placeholderIDs {
		entities = append(entities, placeholderCandidates[id])
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
	// Finding 4: bail out on malformed ref range instead of using refLine=0.
	refLine, ok := occStartLine(ref.Range)
	if !ok {
		return nil, false
	}

	// Finding 3: return the innermost (smallest-span) containing def, not the first.
	var best *scipbindings.Occurrence
	var bestSpan int32 = -1

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
			span := endLine - startLine
			if best == nil || span < bestSpan {
				best = d
				bestSpan = span
			}
		}
	}
	if best != nil {
		return best, true
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

// occLineStartEnd returns LineStart and LineEnd for an entity from a SCIP
// range. SCIP lines are 0-based; LineStart is converted to 1-based to match the
// contract convention (AST analyzers also emit 1-based LineStart).
//
// The end line is computed differently per range kind so it is never less than
// LineStart: for an EnclosingRange the 0-based exclusive end already equals the
// 1-based inclusive last line, but for a single-token name Range (the fallback
// when no EnclosingRange exists) the inclusive 0-based end would yield
// LineEnd == LineStart-1; we clamp LineEnd to at least LineStart so a
// single-line entity reports LineStart == LineEnd rather than an inverted span.
// Returns (0, 0) when the range is malformed.
func occLineStartEnd(r []int32) (lineStart, lineEnd int) {
	start, ok1 := occStartLine(r)
	end, ok2 := occEndLine(r)
	if !ok1 || !ok2 {
		return 0, 0
	}
	lineStart = int(start) + 1
	lineEnd = int(end)
	if lineEnd < lineStart {
		lineEnd = lineStart
	}
	return lineStart, lineEnd
}
