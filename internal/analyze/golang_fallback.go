package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	golang "github.com/smacker/go-tree-sitter/golang"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// fallbackAnalyzeGoPackage parses the in-scope files of a broken Go package with
// tree-sitter and emits go_func entities, defines edges, chunks, and name-based
// calls edges. All call edges carry degraded_by=no_typecheck and confidence <= 0.45.
//
// modulePath is the module path read from go.mod (e.g. "example.com/broken").
// absRepoRoot is the absolute path to the repository root.
// files is the full analyzer scope (repo-relative paths).
// pkgFiles are the absolute paths of files belonging to this broken package.
func fallbackAnalyzeGoPackage(
	log *slog.Logger,
	modulePath string,
	absRepoRoot string,
	scope map[string]bool,
	pkgFiles []string,
) Result {
	lang := golang.GetLanguage()

	type parsedFile struct {
		relPath string
		pkgPath string
		src     []byte
		root    *sitter.Node
	}

	var res Result

	parsed := make([]parsedFile, 0, len(pkgFiles))
	for _, absPath := range pkgFiles {
		rel := repoRelPath(absRepoRoot, absPath)
		if !scope[rel] {
			continue
		}
		src, err := os.ReadFile(absPath) //nolint:gosec
		if err != nil {
			log.Warn("fallback: cannot read file", slog.String("file", rel), slog.String("err", err.Error()))
			continue
		}
		root, err := sitter.ParseCtx(context.Background(), src, lang)
		if err != nil {
			log.Warn("fallback: tree-sitter parse error", slog.String("file", rel), slog.String("err", err.Error()))
			res.ParseErrors++
			continue
		}
		pkgPath := fallbackPkgPath(modulePath, absRepoRoot, absPath)
		parsed = append(parsed, parsedFile{relPath: rel, pkgPath: pkgPath, src: src, root: root})
	}

	if len(parsed) == 0 {
		return res
	}

	// Build a package-wide name -> entityID map for intra-package call resolution.
	pkgDefs := map[string]string{}
	for _, pf := range parsed {
		collectFuncDefs(pf.pkgPath, pf.root, pf.src, pkgDefs)
	}

	for _, pf := range parsed {
		emitFallbackFile(log, pf.relPath, pf.pkgPath, pf.src, pf.root, pkgDefs, &res)
	}

	return res
}

// fallbackPkgPath computes the Go package path for a file structurally from the
// module path and the file's directory relative to the module root (absRepoRoot).
func fallbackPkgPath(modulePath, absRepoRoot, absFilePath string) string {
	dir := filepath.Dir(absFilePath)
	rel, err := filepath.Rel(absRepoRoot, dir)
	if err != nil || rel == "." {
		return modulePath
	}
	return modulePath + "/" + rel
}

// collectFuncDefs walks a parsed source_file node and populates defs with
// function name -> entityID for all function_declarations in the file.
func collectFuncDefs(pkgPath string, root *sitter.Node, src []byte, defs map[string]string) {
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			defs[name] = "go:func:" + pkgPath + "." + name
		case "method_declaration":
			nameNode := child.ChildByFieldName("name")
			recvNode := child.ChildByFieldName("receiver")
			if nameNode == nil || recvNode == nil {
				continue
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			recv := fallbackReceiverName(recvNode, src)
			defs[name] = "go:method:" + pkgPath + ".(" + recv + ")." + name
		}
	}
}

// fallbackReceiverName extracts the base type name from a receiver node.
// The receiver node is a parameter_list; the first parameter's type is the receiver type.
func fallbackReceiverName(recvNode *sitter.Node, src []byte) string {
	// receiver node: parameter_list -> parameter_declaration -> type
	count := int(recvNode.NamedChildCount())
	for i := 0; i < count; i++ {
		param := recvNode.NamedChild(i)
		if param == nil {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		t := typeNode.Type()
		switch t {
		case "pointer_type":
			inner := typeNode.NamedChild(0)
			if inner != nil {
				return string(src[inner.StartByte():inner.EndByte()])
			}
		case "type_identifier":
			return string(src[typeNode.StartByte():typeNode.EndByte()])
		}
	}
	return "unknown"
}

// emitFallbackFile emits entities, edges, and chunks for one file parsed by the fallback.
func emitFallbackFile(
	log *slog.Logger,
	relPath, pkgPath string,
	src []byte,
	root *sitter.Node,
	pkgDefs map[string]string,
	res *Result,
) {
	pkgID := "go:package:" + pkgPath

	// Emit package entity (may be a duplicate if multiple files share a package,
	// but the contract allows duplicate entity IDs - upsert on server side).
	res.Entities = append(res.Entities, contract.Entity{
		ID:   pkgID,
		Name: fallbackPackageName(root, src),
		Type: contract.EntityGoPackage,
	})

	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_declaration":
			emitFallbackFunc(log, relPath, pkgPath, pkgID, src, child, pkgDefs, res)
		case "method_declaration":
			emitFallbackMethod(log, relPath, pkgPath, pkgID, src, child, pkgDefs, res)
		}
	}
}

// fallbackPackageName extracts the package identifier from a source_file root node.
func fallbackPackageName(root *sitter.Node, src []byte) string {
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		if child == nil {
			continue
		}
		if child.Type() == "package_clause" {
			nameNode := child.NamedChild(0)
			if nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
		}
	}
	return ""
}

// emitFallbackFunc emits a go_func entity + defines edge + chunk + call edges for one
// function_declaration node.
func emitFallbackFunc(
	_ *slog.Logger,
	relPath, pkgPath, pkgID string,
	src []byte,
	node *sitter.Node,
	pkgDefs map[string]string,
	res *Result,
) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	entityID := "go:func:" + pkgPath + "." + name

	props := map[string]string{
		"line_start": fmt.Sprintf("%d", node.StartPoint().Row+1),
		"line_end":   fmt.Sprintf("%d", node.EndPoint().Row+1),
		"exported":   fmt.Sprintf("%v", isExported(name)),
	}

	res.Entities = append(res.Entities, contract.Entity{
		ID:         entityID,
		Name:       name,
		Type:       contract.EntityGoFunc,
		FilePath:   relPath,
		Properties: props,
	})

	if isExported(name) {
		res.Symbols = append(res.Symbols, contract.SymbolRow{
			Symbol:   pkgPath + "." + name,
			Lang:     "go",
			Kind:     "func",
			Role:     contract.RoleProvides,
			EntityID: entityID,
			SrcFile:  relPath,
		})
	}

	res.Edges = append(res.Edges, contract.Edge{
		From:     pkgID,
		To:       entityID,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})

	body := node.ChildByFieldName("body")
	if body == nil {
		body = node.ChildByFieldName("block")
	}

	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: entityID,
		Type:     contract.EntityGoFunc,
		FilePath: relPath,
		Language: "go",
		Header:   fmt.Sprintf("[go_func] %s.%s\nfile: %s\npackage: %s", pkgPath, name, relPath, pkgPath),
		Body:     string(src[node.StartByte():node.EndByte()]),
	})

	// Emit name-based call edges (degraded).
	if body != nil {
		emitFallbackCallEdges(relPath, entityID, src, body, pkgDefs, res)
	}
}

// emitFallbackMethod emits a go_method entity + defines edge + chunk + call edges for one
// method_declaration node.
func emitFallbackMethod(
	_ *slog.Logger,
	relPath, pkgPath, pkgID string,
	src []byte,
	node *sitter.Node,
	pkgDefs map[string]string,
	res *Result,
) {
	nameNode := node.ChildByFieldName("name")
	recvNode := node.ChildByFieldName("receiver")
	if nameNode == nil || recvNode == nil {
		return
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	recv := fallbackReceiverName(recvNode, src)
	entityID := "go:method:" + pkgPath + ".(" + recv + ")." + name

	props := map[string]string{
		"line_start": fmt.Sprintf("%d", node.StartPoint().Row+1),
		"line_end":   fmt.Sprintf("%d", node.EndPoint().Row+1),
		"exported":   fmt.Sprintf("%v", isExported(name)),
	}

	res.Entities = append(res.Entities, contract.Entity{
		ID:         entityID,
		Name:       name,
		Type:       contract.EntityGoMethod,
		FilePath:   relPath,
		Properties: props,
	})

	if isExported(name) {
		res.Symbols = append(res.Symbols, contract.SymbolRow{
			Symbol:   pkgPath + "." + recv + "." + name,
			Lang:     "go",
			Kind:     "method",
			Role:     contract.RoleProvides,
			EntityID: entityID,
			SrcFile:  relPath,
		})
	}

	res.Edges = append(res.Edges, contract.Edge{
		From:     pkgID,
		To:       entityID,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})

	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: entityID,
		Type:     contract.EntityGoMethod,
		FilePath: relPath,
		Language: "go",
		Header:   fmt.Sprintf("[go_method] %s.(%s).%s\nfile: %s\npackage: %s", pkgPath, recv, name, relPath, pkgPath),
		Body:     string(src[node.StartByte():node.EndByte()]),
	})

	body := node.ChildByFieldName("body")
	if body == nil {
		body = node.ChildByFieldName("block")
	}
	if body != nil {
		emitFallbackCallEdges(relPath, entityID, src, body, pkgDefs, res)
	}
}

// emitFallbackCallEdges walks a block/body node for call_expression nodes and emits
// degraded calls edges when the callee name is found in pkgDefs.
func emitFallbackCallEdges(
	relPath, callerID string,
	src []byte,
	body *sitter.Node,
	pkgDefs map[string]string,
	res *Result,
) {
	seenEdge := map[string]bool{}

	walkSitterCalls(body, src, func(calleeName string) {
		calleeID, ok := pkgDefs[calleeName]
		if !ok {
			return
		}
		key := callerID + "->" + calleeID
		if seenEdge[key] {
			return
		}
		seenEdge[key] = true

		// Use scoped_name_match but always cap at 0.45 for the fallback.
		rawConf := contract.ConfidenceFor(contract.ResScopedNameMatch)
		conf := rawConf
		if conf > "0.45" {
			conf = "0.45"
		}

		res.Edges = append(res.Edges, contract.Edge{
			From:     callerID,
			To:       calleeID,
			Relation: contract.RelCalls,
			SrcFile:  relPath,
			Properties: map[string]string{
				"resolution":  contract.ResScopedNameMatch,
				"confidence":  conf,
				"degraded_by": "no_typecheck",
			},
		})
	})
}

// walkSitterCalls recursively visits all call_expression nodes and calls fn with
// the callee name (identifier only; selector/attribute calls are skipped).
func walkSitterCalls(node *sitter.Node, src []byte, fn func(calleeName string)) {
	if node == nil {
		return
	}
	if node.Type() == "call_expression" {
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil && funcNode.Type() == "identifier" {
			fn(string(src[funcNode.StartByte():funcNode.EndByte()]))
		}
	}
	count := int(node.ChildCount())
	for i := 0; i < count; i++ {
		walkSitterCalls(node.Child(i), src, fn)
	}
}

// isExported reports whether a Go identifier is exported (starts with uppercase).
func isExported(name string) bool {
	if name == "" {
		return false
	}
	return strings.ToUpper(name[:1]) == name[:1] && name[:1] != "_"
}
