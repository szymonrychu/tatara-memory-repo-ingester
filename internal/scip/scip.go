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
		fileSet  = make(map[string]struct{})
	)

	for _, doc := range idx.Documents {
		if doc == nil {
			continue
		}
		fileSet[doc.RelativePath] = struct{}{}
		lang := strings.ToLower(doc.Language)

		// Index symbol metadata by symbol string.
		symInfo := make(map[string]*scipbindings.SymbolInformation, len(doc.Symbols))
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
			si, ok := symInfo[d.Symbol]
			if !ok {
				// No SymbolInformation: emit a minimal entity.
				entities = append(entities, contract.Entity{
					ID:       entityID(lang, d.Symbol),
					Name:     lastComponent(d.Symbol),
					Type:     "scip_symbol",
					FilePath: doc.RelativePath,
					Properties: map[string]string{
						"scip_kind": "unspecified",
					},
				})
				continue
			}
			kind := si.Kind
			entities = append(entities, contract.Entity{
				ID:       entityID(lang, si.Symbol),
				Name:     displayName(si),
				Type:     scipKind(kind),
				FilePath: doc.RelativePath,
				Properties: map[string]string{
					"scip_kind": kind.String(),
				},
			})
		}

		// Emit edges: each reference occurrence -> find enclosing def -> emit edge.
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
			edges = append(edges, contract.Edge{
				From:     entityID(lang, enclosing.Symbol),
				To:       entityID(lang, ref.Symbol),
				Relation: rel,
				SrcFile:  doc.RelativePath,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
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

// lastComponent returns the last meaningful component of a SCIP symbol string.
func lastComponent(sym string) string {
	for _, sep := range []string{"/", ".", "`"} {
		if idx := strings.LastIndex(sym, sep); idx >= 0 && idx < len(sym)-1 {
			rest := sym[idx+1:]
			// strip trailing punctuation like () or .
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

// enclosingDef finds the definition occurrence whose range contains the start
// of ref. A 4-element range is [startLine, startChar, endLine, endChar];
// a 3-element range is [startLine, startChar, endChar] (same line).
func enclosingDef(defs []*scipbindings.Occurrence, ref *scipbindings.Occurrence) (*scipbindings.Occurrence, bool) {
	refLine, _ := occStartLine(ref.Range)
	for _, d := range defs {
		if d.Symbol == ref.Symbol {
			continue // skip self-reference
		}
		startLine, ok1 := occStartLine(d.Range)
		endLine, ok2 := occEndLine(d.Range)
		if !ok1 || !ok2 {
			continue
		}
		if refLine >= startLine && refLine <= endLine {
			return d, true
		}
	}
	return nil, false
}

func occStartLine(r []int32) (int32, bool) {
	if len(r) < 2 {
		return 0, false
	}
	return r[0], true
}

func occEndLine(r []int32) (int32, bool) {
	switch len(r) {
	case 3:
		// [startLine, startChar, endChar] - single line
		return r[0], true
	case 4:
		return r[2], true
	default:
		return 0, false
	}
}
