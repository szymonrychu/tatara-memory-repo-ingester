package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/javascript"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// jsAnalyzer implements Analyzer for JavaScript source files using tree-sitter.
type jsAnalyzer struct {
	log *slog.Logger
}

// NewJavaScript returns an Analyzer for JavaScript source files.
func NewJavaScript() Analyzer {
	return jsAnalyzer{log: slog.Default()}
}

// Name returns the analyzer name.
func (j jsAnalyzer) Name() string { return "javascript" }

// Match reports whether the path is a JavaScript source file.
func (j jsAnalyzer) Match(path string) bool {
	return strings.HasSuffix(path, ".js") ||
		strings.HasSuffix(path, ".mjs") ||
		strings.HasSuffix(path, ".cjs")
}

// Analyze parses each file with tree-sitter and emits entities, edges, and chunks.
// The repo-wide resolution index is built from ALL .js/.mjs/.cjs files found under repoRoot
// so that incremental ingest (where `files` is a diff subset) still resolves cross-file edges.
// Entities, edges, and chunks are only emitted for files in `files`.
func (j jsAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	lang := javascript.GetLanguage()

	type parsedFile struct {
		relPath string
		src     []byte
		root    *sitter.Node
	}

	var res Result

	// Walk repoRoot for all JS files to populate the repo-wide index.
	allJSFiles := jsWalkRepo(repoRoot)

	diffSet := make(map[string]bool, len(files))
	for _, f := range files {
		diffSet[f] = true
	}

	allParsed := make([]parsedFile, 0, len(allJSFiles))
	for _, relPath := range allJSFiles {
		absPath := filepath.Join(repoRoot, relPath)
		src, err := os.ReadFile(absPath) //nolint:gosec
		if err != nil {
			return Result{}, fmt.Errorf("read %q: %w", absPath, err)
		}
		root, err := sitter.ParseCtx(context.Background(), src, lang)
		if err != nil {
			j.log.Warn("tree-sitter parse error", slog.String("file", relPath), slog.String("err", err.Error()))
			if diffSet[relPath] {
				res.ParseErrors++
			}
			continue
		}
		allParsed = append(allParsed, parsedFile{
			relPath: relPath,
			src:     src,
			root:    root,
		})
	}

	// Build repo-wide name -> []entityID index and O(1) module-path set from ALL files.
	repoIndex := map[string][]string{}
	moduleSet := make(map[string]bool, len(allParsed))
	for _, pf := range allParsed {
		moduleSet[pf.relPath] = true
		defs := jsFileDefs(pf.relPath, pf.root, pf.src)
		for name, id := range defs {
			repoIndex[name] = append(repoIndex[name], id)
		}
	}

	// Emit entities/edges/chunks only for files in the diff set.
	for _, pf := range allParsed {
		if !diffSet[pf.relPath] {
			continue
		}
		moduleDefs := jsFileDefs(pf.relPath, pf.root, pf.src)
		importMap := jsImportMap(pf.relPath, pf.root, pf.src, repoIndex, moduleSet)
		j.processFile(pf.relPath, pf.src, pf.root, moduleDefs, importMap, repoIndex, moduleSet, &res)
	}

	return res, nil
}

// jsWalkRepo returns all repo-relative JS file paths under root.
func jsWalkRepo(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs") {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	return out
}

// moduleID returns the js:module entity ID for a repo-relative path.
func moduleID(relPath string) string {
	return "js:module:" + relPath
}

// funcID returns the js:func entity ID.
func funcID(relPath, name string) string {
	return "js:func:" + relPath + "::" + name
}

// classID returns the js:class entity ID.
func classID(relPath, name string) string {
	return "js:class:" + relPath + "::" + name
}

// jsFileDefs collects all function and class names defined in a JS file,
// returning name -> entityID for M3 resolution.
func jsFileDefs(relPath string, root *sitter.Node, src []byte) map[string]string {
	defs := map[string]string{}
	count := int(root.ChildCount())
	for i := 0; i < count; i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "function_declaration", "export_statement":
			// export_statement wraps function_declaration or class_declaration
			inner := child
			if child.Type() == "export_statement" {
				inner = child.NamedChild(0)
				if inner == nil {
					continue
				}
			}
			switch inner.Type() {
			case "function_declaration":
				if nameNode := inner.ChildByFieldName("name"); nameNode != nil {
					n := nameNode.Content(src)
					defs[n] = funcID(relPath, n)
				}
			case "class_declaration":
				if nameNode := inner.ChildByFieldName("name"); nameNode != nil {
					n := nameNode.Content(src)
					defs[n] = classID(relPath, n)
				}
			case "lexical_declaration", "variable_declaration":
				jsCollectArrowDefs(inner, relPath, src, defs)
			}
		case "class_declaration":
			if nameNode := child.ChildByFieldName("name"); nameNode != nil {
				n := nameNode.Content(src)
				defs[n] = classID(relPath, n)
			}
		case "lexical_declaration":
			// const f = () => ...  or  const f = function() { ... }
			jsCollectArrowDefs(child, relPath, src, defs)
		}
	}
	return defs
}

// jsCollectArrowDefs extracts arrow function and function expression names from a lexical_declaration.
func jsCollectArrowDefs(node *sitter.Node, relPath string, src []byte, defs map[string]string) {
	nc := int(node.NamedChildCount())
	for i := 0; i < nc; i++ {
		decl := node.NamedChild(i)
		if decl == nil || decl.Type() != "variable_declarator" {
			continue
		}
		valNode := decl.ChildByFieldName("value")
		if valNode == nil {
			continue
		}
		if valNode.Type() == "arrow_function" || valNode.Type() == "function_expression" {
			nameNode := decl.ChildByFieldName("name")
			if nameNode != nil {
				n := nameNode.Content(src)
				defs[n] = funcID(relPath, n)
			}
		}
	}
}

// jsImportMap builds local-name -> entityID map from ES import statements and CommonJS require() calls.
// Only names that resolve to a known entity in repoIndex or a known module in moduleSet are included.
func jsImportMap(relPath string, root *sitter.Node, src []byte, repoIndex map[string][]string, moduleSet map[string]bool) map[string]string {
	imports := map[string]string{}
	fileDir := filepath.Dir(relPath)

	count := int(root.ChildCount())
	for i := 0; i < count; i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import_statement":
			// import { x, y } from './module.js'
			// AST: import_statement(import_clause(named_imports(import_specifier...)), source: string)
			sourceNode := child.ChildByFieldName("source")
			if sourceNode == nil {
				continue
			}
			// source node is a "string" containing quotes; extract inner string_fragment if present.
			rawSource := jsStringValue(sourceNode, src)
			resolved := resolveModulePath(fileDir, rawSource)

			// Find import_clause among named children (it has no field name).
			var clauseNode *sitter.Node
			for ci := 0; ci < int(child.NamedChildCount()); ci++ {
				nc := child.NamedChild(ci)
				if nc != nil && nc.Type() == "import_clause" {
					clauseNode = nc
					break
				}
			}
			if clauseNode == nil {
				continue
			}
			// import_clause may contain named_imports directly.
			jsCollectImportedNames(clauseNode, resolved, src, repoIndex, imports)

		case "expression_statement":
			// CommonJS: const x = require('./module')
			// These appear as expression_statement > assignment/call.
			// We look for require() calls in variable declarations too.
		case "lexical_declaration", "variable_declaration":
			// const x = require('./module')
			jsCollectRequireImports(child, fileDir, src, moduleSet, imports)
		}
	}
	return imports
}

// jsCollectImportedNames walks an import clause node and maps each imported
// identifier to the corresponding entity in the target module.
// clauseNode is an import_clause, which may contain named_imports or an identifier.
func jsCollectImportedNames(clauseNode *sitter.Node, targetPath string, src []byte, repoIndex map[string][]string, imports map[string]string) {
	// import_clause can directly be an identifier (default import) or wrap named_imports.
	count := int(clauseNode.NamedChildCount())
	for i := 0; i < count; i++ {
		child := clauseNode.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "named_imports":
			// { x, y } - iterate import_specifier children.
			jsCollectNamedImports(child, targetPath, src, repoIndex, imports)
		case "identifier":
			// import defaultExport from '...'
			name := child.Content(src)
			if id, ok := jsMatchImport(targetPath, name, repoIndex); ok {
				imports[name] = id
			}
		case "namespace_import":
			// import * as ns from '...' - skip
		}
	}
}

// jsCollectNamedImports processes a named_imports node ({ x, y }).
func jsCollectNamedImports(namedImports *sitter.Node, targetPath string, src []byte, repoIndex map[string][]string, imports map[string]string) {
	count := int(namedImports.NamedChildCount())
	for i := 0; i < count; i++ {
		spec := namedImports.NamedChild(i)
		if spec == nil || spec.Type() != "import_specifier" {
			continue
		}
		// import_specifier: name field is the imported name (may differ from local alias).
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			// Fallback: first named child is the identifier.
			if spec.NamedChildCount() > 0 {
				nameNode = spec.NamedChild(0)
			}
		}
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(src)
		if id, ok := jsMatchImport(targetPath, name, repoIndex); ok {
			imports[name] = id
		}
	}
}

// jsMatchImport looks up name in repoIndex and returns the entity ID from targetPath
// that matches, accepting both js:func: and js:class: kinds.
func jsMatchImport(targetPath, name string, repoIndex map[string][]string) (string, bool) {
	funcCandidate := funcID(targetPath, name)
	classCandidate := classID(targetPath, name)
	for _, id := range repoIndex[name] {
		if id == funcCandidate || id == classCandidate {
			return id, true
		}
	}
	return "", false
}

// jsStringValue extracts the string value from a tree-sitter string node,
// preferring the string_fragment child over stripping quotes from the raw content.
func jsStringValue(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	// Look for string_fragment child.
	count := int(node.ChildCount())
	for i := 0; i < count; i++ {
		c := node.Child(i)
		if c != nil && c.Type() == "string_fragment" {
			return c.Content(src)
		}
	}
	// Fallback: strip surrounding quotes.
	return strings.Trim(node.Content(src), "'\"` ")
}

// jsCollectRequireImports extracts CommonJS require() bindings from variable declarations.
// It uses moduleSet for O(1) existence checks instead of scanning repoIndex values.
func jsCollectRequireImports(node *sitter.Node, fileDir string, src []byte, moduleSet map[string]bool, imports map[string]string) {
	nc := int(node.NamedChildCount())
	for i := 0; i < nc; i++ {
		decl := node.NamedChild(i)
		if decl == nil || decl.Type() != "variable_declarator" {
			continue
		}
		val := decl.ChildByFieldName("value")
		if val == nil || val.Type() != "call_expression" {
			continue
		}
		fnNode := val.ChildByFieldName("function")
		if fnNode == nil || fnNode.Content(src) != "require" {
			continue
		}
		argsNode := val.ChildByFieldName("arguments")
		if argsNode == nil || argsNode.NamedChildCount() == 0 {
			continue
		}
		argNode := argsNode.NamedChild(0)
		if argNode == nil || (argNode.Type() != "string" && argNode.Type() != "template_string") {
			continue
		}
		rawSource := jsStringValue(argNode, src)
		resolved := resolveModulePath(fileDir, rawSource)

		// Only record the binding if the target module is known in this repo.
		if !moduleSet[resolved] {
			continue
		}
		nameNode := decl.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		imports[nameNode.Content(src)] = moduleID(resolved)
	}
}

// resolveModulePath resolves a module specifier relative to the importing file's dir,
// appending ".js" if no extension is present.
func resolveModulePath(fileDir, specifier string) string {
	if !strings.HasPrefix(specifier, ".") {
		// bare specifier - not resolvable to repo-relative path
		return specifier
	}
	joined := filepath.Join(fileDir, specifier)
	// Normalize to forward slashes (repo paths use /).
	joined = filepath.ToSlash(joined)
	if !strings.Contains(filepath.Base(joined), ".") {
		joined += ".js"
	}
	return joined
}

// processFile emits all entities, edges, and chunks for one JavaScript file.
func (j jsAnalyzer) processFile(
	relPath string,
	src []byte,
	root *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	repoIndex map[string][]string,
	moduleSet map[string]bool,
	res *Result,
) {
	modID := moduleID(relPath)

	res.Entities = append(res.Entities, contract.Entity{
		ID:       modID,
		Name:     relPath,
		Type:     contract.EntityJSModule,
		FilePath: relPath,
	})
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: modID,
		Type:     contract.EntityJSModule,
		FilePath: relPath,
		Language: "javascript",
		Header:   fmt.Sprintf("[js_module] %s\nfile: %s", relPath, relPath),
		Body:     string(src),
	})

	// Emit require()-based import edges: importMap values that are js:module: IDs
	// came from CommonJS require() bindings resolved by jsCollectRequireImports.
	emittedImports := map[string]bool{}
	for _, targetID := range importMap {
		if strings.HasPrefix(targetID, "js:module:") && !emittedImports[targetID] {
			res.Edges = append(res.Edges, contract.Edge{
				From:     modID,
				To:       targetID,
				Relation: contract.RelImports,
				SrcFile:  relPath,
			})
			emittedImports[targetID] = true
		}
	}

	// Single pass over top-level nodes: emit import edges and requires SymbolRows
	// for external specifiers inline, avoiding a second walk via jsExternalImports.
	fileDir := filepath.Dir(relPath)
	seenExternal := map[string]bool{}
	count := int(root.ChildCount())
	for i := 0; i < count; i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import_statement":
			srcNode := child.ChildByFieldName("source")
			if srcNode == nil {
				continue
			}
			rawSource := jsStringValue(srcNode, src)
			resolved := resolveModulePath(fileDir, rawSource)
			if strings.HasPrefix(rawSource, ".") {
				res.Edges = append(res.Edges, contract.Edge{
					From:     modID,
					To:       moduleID(resolved),
					Relation: contract.RelImports,
					SrcFile:  relPath,
				})
			} else if !seenExternal[rawSource] {
				// Bare specifier not in-repo -> requires SymbolRow.
				seenExternal[rawSource] = true
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   rawSource,
					Lang:     "javascript",
					Kind:     "module",
					Role:     contract.RoleRequires,
					EntityID: modID,
					SrcFile:  relPath,
				})
			}

		case "function_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			j.emitFunc(relPath, nameNode.Content(src), false, src, child, moduleDefs, importMap, repoIndex, modID, res)

		case "export_statement":
			inner := child.NamedChild(0)
			if inner == nil {
				continue
			}
			switch inner.Type() {
			case "function_declaration":
				nameNode := inner.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				name := nameNode.Content(src)
				j.emitFunc(relPath, name, false, src, inner, moduleDefs, importMap, repoIndex, modID, res)
				// provides SymbolRow for exported function.
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   relPath + "::" + name,
					Lang:     "javascript",
					Kind:     "func",
					Role:     contract.RoleProvides,
					EntityID: funcID(relPath, name),
					SrcFile:  relPath,
				})
			case "class_declaration":
				nameNode := inner.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				name := nameNode.Content(src)
				j.emitClass(relPath, name, src, inner, modID, res)
				// provides SymbolRow for exported class.
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   relPath + "::" + name,
					Lang:     "javascript",
					Kind:     "class",
					Role:     contract.RoleProvides,
					EntityID: classID(relPath, name),
					SrcFile:  relPath,
				})
			case "lexical_declaration", "variable_declaration":
				// export const handler = () => {}  or  export const helper = function() {}
				nc := int(inner.NamedChildCount())
				for di := 0; di < nc; di++ {
					decl := inner.NamedChild(di)
					if decl == nil || decl.Type() != "variable_declarator" {
						continue
					}
					valNode := decl.ChildByFieldName("value")
					if valNode == nil {
						continue
					}
					if valNode.Type() != "arrow_function" && valNode.Type() != "function_expression" {
						continue
					}
					nameNode := decl.ChildByFieldName("name")
					if nameNode == nil {
						continue
					}
					name := nameNode.Content(src)
					j.emitFunc(relPath, name, false, src, valNode, moduleDefs, importMap, repoIndex, modID, res)
					res.Symbols = append(res.Symbols, contract.SymbolRow{
						Symbol:   relPath + "::" + name,
						Lang:     "javascript",
						Kind:     "func",
						Role:     contract.RoleProvides,
						EntityID: funcID(relPath, name),
						SrcFile:  relPath,
					})
				}
			}

		case "class_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			j.emitClass(relPath, nameNode.Content(src), src, child, modID, res)

		case "lexical_declaration", "variable_declaration":
			// const f = () => ...
			j.emitArrowFuncs(relPath, src, child, moduleDefs, importMap, repoIndex, modID, res)
		}
	}
}

// emitClass emits a js_class entity.
func (j jsAnalyzer) emitClass(relPath, name string, src []byte, node *sitter.Node, parentID string, res *Result) {
	cid := classID(relPath, name)
	res.Entities = append(res.Entities, contract.Entity{
		ID:       cid,
		Name:     name,
		Type:     contract.EntityJSClass,
		FilePath: relPath,
	})
	res.Edges = append(res.Edges, contract.Edge{
		From:     parentID,
		To:       cid,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: cid,
		Type:     contract.EntityJSClass,
		FilePath: relPath,
		Language: "javascript",
		Header:   fmt.Sprintf("[js_class] %s::%s\nfile: %s", relPath, name, relPath),
		Body:     node.Content(src),
	})
}

// emitArrowFuncs emits js_func entities for arrow functions bound via lexical_declaration.
func (j jsAnalyzer) emitArrowFuncs(
	relPath string,
	src []byte,
	node *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	repoIndex map[string][]string,
	parentID string,
	res *Result,
) {
	nc := int(node.NamedChildCount())
	for i := 0; i < nc; i++ {
		decl := node.NamedChild(i)
		if decl == nil || decl.Type() != "variable_declarator" {
			continue
		}
		valNode := decl.ChildByFieldName("value")
		if valNode == nil {
			continue
		}
		if valNode.Type() != "arrow_function" && valNode.Type() != "function_expression" {
			continue
		}
		nameNode := decl.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		j.emitFunc(relPath, nameNode.Content(src), false, src, valNode, moduleDefs, importMap, repoIndex, parentID, res)
	}
}

// emitFunc emits a js_func entity and its call edges.
func (j jsAnalyzer) emitFunc(
	relPath, name string,
	isDynamic bool,
	src []byte,
	node *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	repoIndex map[string][]string,
	parentID string,
	res *Result,
) {
	fid := funcID(relPath, name)

	res.Entities = append(res.Entities, contract.Entity{
		ID:       fid,
		Name:     name,
		Type:     contract.EntityJSFunc,
		FilePath: relPath,
	})
	res.Edges = append(res.Edges, contract.Edge{
		From:     parentID,
		To:       fid,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: fid,
		Type:     contract.EntityJSFunc,
		FilePath: relPath,
		Language: "javascript",
		Header:   fmt.Sprintf("[js_func] %s::%s\nfile: %s", relPath, name, relPath),
		Body:     node.Content(src),
	})

	// Body: for function_declaration the "body" field is the block;
	// for arrow_function it's either an expression or a statement_block.
	bodyNode := node.ChildByFieldName("body")
	if bodyNode == nil {
		// Some arrow functions have expression body without a "body" field name;
		// walk the whole node for calls.
		bodyNode = node
	}

	danglingCalls := []string{}
	degradedReasons := []string{}
	seenEdge := map[string]bool{}

	jsWalkCalls(bodyNode, src, func(callNode *sitter.Node, callee *sitter.Node) {
		// Detect dynamic/computed calls: subscript_expression (obj[expr]()) or
		// call_expression where function is subscript_expression.
		if callee.Type() == "subscript_expression" {
			// Dynamic: obj[expr]() - cannot resolve statically.
			danglingCalls = append(danglingCalls, callee.Content(src))
			degradedReasons = append(degradedReasons, "dynamic")
			return
		}

		// member_expression call (obj.method): treat as dangling.
		if callee.Type() == "member_expression" {
			danglingCalls = append(danglingCalls, callee.Content(src))
			return
		}

		calleeName := callee.Content(src)

		// Tier 1: scoped_name_match - defined in this module.
		if calleeID, ok := moduleDefs[calleeName]; ok {
			edgeKey := fid + "->" + calleeID
			if seenEdge[edgeKey] {
				return
			}
			seenEdge[edgeKey] = true
			props := map[string]string{
				"resolution": contract.ResScopedNameMatch,
				"confidence": contract.ConfidenceFor(contract.ResScopedNameMatch),
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:       fid,
				To:         calleeID,
				Relation:   contract.RelCalls,
				SrcFile:    relPath,
				Properties: props,
			})
			return
		}

		// Tier 2: imported_name_match - resolves via tracked import.
		if calleeID, ok := importMap[calleeName]; ok {
			edgeKey := fid + "->" + calleeID
			if seenEdge[edgeKey] {
				return
			}
			seenEdge[edgeKey] = true
			props := map[string]string{
				"resolution": contract.ResImportedNameMatch,
				"confidence": contract.ConfidenceFor(contract.ResImportedNameMatch),
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:       fid,
				To:         calleeID,
				Relation:   contract.RelCalls,
				SrcFile:    relPath,
				Properties: props,
			})
			return
		}

		// Tier 3/4: global or ambiguous.
		ids := repoIndex[calleeName]
		switch len(ids) {
		case 0:
			danglingCalls = append(danglingCalls, calleeName)
		case 1:
			edgeKey := fid + "->" + ids[0]
			if !seenEdge[edgeKey] {
				seenEdge[edgeKey] = true
				props := map[string]string{
					"resolution": contract.ResGlobalNameMatch,
					"confidence": contract.ConfidenceFor(contract.ResGlobalNameMatch),
				}
				res.Edges = append(res.Edges, contract.Edge{
					From:       fid,
					To:         ids[0],
					Relation:   contract.RelCalls,
					SrcFile:    relPath,
					Properties: props,
				})
			}
		default:
			for _, id := range ids {
				edgeKey := fid + "->" + id
				if seenEdge[edgeKey] {
					continue
				}
				seenEdge[edgeKey] = true
				props := map[string]string{
					"resolution": contract.ResAmbiguousMultiDef,
					"confidence": contract.ConfidenceFor(contract.ResAmbiguousMultiDef),
				}
				res.Edges = append(res.Edges, contract.Edge{
					From:       fid,
					To:         id,
					Relation:   contract.RelCalls,
					SrcFile:    relPath,
					Properties: props,
				})
			}
		}
	})

	// Attach dangling_call and degraded_by to the entity.
	if len(danglingCalls) > 0 || len(degradedReasons) > 0 {
		for i, e := range res.Entities {
			if e.ID == fid {
				if res.Entities[i].Properties == nil {
					res.Entities[i].Properties = map[string]string{}
				}
				if len(danglingCalls) > 0 {
					res.Entities[i].Properties["dangling_call"] = strings.Join(danglingCalls, ",")
				}
				if len(degradedReasons) > 0 {
					res.Entities[i].Properties["degraded_by"] = strings.Join(degradedReasons, ",")
				}
				break
			}
		}
	}
}

// jsWalkCalls walks the node tree and calls fn for every call_expression or new_expression node
// along with its function/constructor/callee child.
func jsWalkCalls(node *sitter.Node, src []byte, fn func(callNode *sitter.Node, callee *sitter.Node)) {
	if node == nil {
		return
	}
	switch node.Type() {
	case "call_expression":
		if fnChild := node.ChildByFieldName("function"); fnChild != nil {
			fn(node, fnChild)
		}
	case "new_expression":
		// new Cls() - constructor field holds the class being instantiated.
		if ctorChild := node.ChildByFieldName("constructor"); ctorChild != nil {
			fn(node, ctorChild)
		}
	}
	count := int(node.ChildCount())
	for i := 0; i < count; i++ {
		jsWalkCalls(node.Child(i), src, fn)
	}
}
