package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// pythonAnalyzer implements Analyzer for Python source files using tree-sitter.
type pythonAnalyzer struct {
	log *slog.Logger
}

// NewPython returns an Analyzer for Python source files.
func NewPython() Analyzer {
	return pythonAnalyzer{log: slog.Default()}
}

// Name returns the analyzer name.
func (p pythonAnalyzer) Name() string { return "python" }

// Match reports whether the path is a Python source file.
func (p pythonAnalyzer) Match(path string) bool {
	return strings.HasSuffix(path, ".py")
}

// Analyze parses each file with tree-sitter and emits entities, edges, and chunks.
// The repo-wide resolution index is built from ALL .py files found under repoRoot so that
// incremental ingest (where `files` is a diff subset) still resolves cross-file call edges.
// Entities, edges, and chunks are only emitted for files in `files`.
func (p pythonAnalyzer) Analyze(ctx context.Context, repoRoot string, files []string) (Result, error) {
	lang := python.GetLanguage()

	type parsedFile struct {
		relPath   string
		moduleFQN string
		src       []byte
		root      *sitter.Node
		defs      map[string]string
	}
	var res Result

	// Collect all .py files in repoRoot for building the repo-wide index.
	allPyFiles, err := walkLang(repoRoot, ".py")
	if err != nil {
		return Result{}, fmt.Errorf("walk repo for .py files: %w", err)
	}

	// Parse all repo files to populate the repo-wide name->entityID index.
	diffSet := make(map[string]bool, len(files))
	for _, f := range files {
		diffSet[f] = true
	}

	allParsed := make([]parsedFile, 0, len(allPyFiles))
	for _, relPath := range allPyFiles {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("analyze cancelled: %w", err)
		}
		absPath := filepath.Join(repoRoot, relPath)
		src, err := os.ReadFile(absPath) //nolint:gosec
		if err != nil {
			p.log.Warn("python: read failed; skipping", slog.String("file", relPath), slog.String("err", err.Error()))
			if diffSet[relPath] {
				res.ParseErrors++
			}
			continue
		}
		root, err := sitter.ParseCtx(ctx, src, lang)
		if err != nil {
			p.log.Warn("tree-sitter parse error", slog.String("file", relPath), slog.String("err", err.Error()))
			if diffSet[relPath] {
				res.ParseErrors++
			}
			continue
		}
		moduleFQN := pathToFQN(relPath)
		allParsed = append(allParsed, parsedFile{
			relPath:   relPath,
			moduleFQN: moduleFQN,
			src:       src,
			root:      root,
			defs:      pyFileDefs(moduleFQN, root, src),
		})
	}

	// Build repo-wide name -> []entityID index from ALL parsed files.
	repoIndex := map[string][]string{}
	for _, pf := range allParsed {
		for name, id := range pf.defs {
			repoIndex[name] = append(repoIndex[name], id)
		}
	}

	// Emit entities/edges/chunks only for files in the diff set.
	for _, pf := range allParsed {
		if !diffSet[pf.relPath] {
			continue
		}
		moduleDefs := pf.defs
		importMap := pyImportMap(pf.root, pf.src, repoIndex)
		extImports := pyExternalImports(pf.root, pf.src, repoIndex)
		p.processFile(pf.relPath, pf.moduleFQN, pf.src, pf.root, moduleDefs, importMap, extImports, repoIndex, &res)
	}

	return res, nil
}

// walkLang returns all repo-relative file paths under root with the given suffix.
func walkLang(root, suffix string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && shouldSkipWalkDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// pathToFQN converts a repo-relative path like "pkg/mod.py" to "pkg.mod".
func pathToFQN(relPath string) string {
	s := strings.TrimSuffix(relPath, ".py")
	return strings.ReplaceAll(s, "/", ".")
}

// pyFileDefs collects all top-level and class-method function names in a module,
// returning a set of short names -> entity IDs (for M3 resolution).
func pyFileDefs(moduleFQN string, root *sitter.Node, src []byte) map[string]string {
	defs := map[string]string{}
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "function_definition":
			name := child.ChildByFieldName("name")
			if name != nil {
				n := name.Content(src)
				defs[n] = "py:func:" + moduleFQN + "." + n
			}
		case "decorated_definition":
			// The inner function_definition is a child of decorated_definition.
			inner := child.ChildByFieldName("definition")
			if inner != nil && inner.Type() == "function_definition" {
				name := inner.ChildByFieldName("name")
				if name != nil {
					n := name.Content(src)
					defs[n] = "py:func:" + moduleFQN + "." + n
				}
			}
		case "class_definition":
			className := child.ChildByFieldName("name")
			if className != nil {
				cname := className.Content(src)
				// Register the class itself so pyMatchImport can resolve imported classes.
				defs[cname] = "py:class:" + moduleFQN + "." + cname
				body := child.ChildByFieldName("body")
				if body != nil {
					bc := int(body.NamedChildCount())
					for j := 0; j < bc; j++ {
						meth := body.NamedChild(j)
						if meth.Type() == "function_definition" {
							mn := meth.ChildByFieldName("name")
							if mn != nil {
								mname := mn.Content(src)
								defs[mname] = "py:func:" + moduleFQN + "." + cname + "." + mname
							}
						}
					}
				}
			}
		}
	}
	return defs
}

// pyImportMap builds a local-name -> entityID map from import statements in the file.
// Only names that resolve to a known entity in repoIndex are included.
func pyImportMap(root *sitter.Node, src []byte, repoIndex map[string][]string) map[string]string {
	imports := map[string]string{}
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "import_from_statement":
			// from <module> import <name>[, <name>...]
			// The imported names appear as dotted_name or aliased_import children after "import".
			moduleName := ""
			modNode := child.ChildByFieldName("module_name")
			if modNode != nil {
				moduleName = modNode.Content(src)
			}
			if moduleName == "" {
				continue
			}
			// Relative imports (leading dot) cannot resolve to absolute entity IDs; skip.
			if strings.HasPrefix(moduleName, ".") || modNode.Type() == "relative_import" {
				continue
			}
			// Convert "pkg.helper" -> "pkg.helper"
			moduleFQN := moduleName

			nc := int(child.NamedChildCount())
			for j := 0; j < nc; j++ {
				n := child.NamedChild(j)
				if n == child.ChildByFieldName("module_name") {
					continue
				}
				switch n.Type() {
				case "wildcard_import":
					// "from m import *" - skip; we cannot resolve individual names.
					continue
				case "aliased_import":
					// "from m import a as b" - map alias b -> candidate for a.
					nameField := n.ChildByFieldName("name")
					aliasField := n.ChildByFieldName("alias")
					if nameField == nil || aliasField == nil {
						continue
					}
					importedName := nameField.Content(src)
					aliasName := aliasField.Content(src)
					if id, ok := pyMatchImport(moduleFQN, importedName, repoIndex); ok {
						imports[aliasName] = id
					}
				default:
					// dotted_name or identifier: plain name import.
					localName := n.Content(src)
					if id, ok := pyMatchImport(moduleFQN, localName, repoIndex); ok {
						imports[localName] = id
					}
				}
			}

		case "import_statement":
			// import <module> [as <alias>]
			// We track "import pkg.helper" -> not providing a local function name directly.
			// Only aliased imports are commonly used for calls; skip bare module imports here
			// since the call would be "module.func()" (attribute), not "func()".
		}
	}
	return imports
}

// pyMatchImport looks up importedName in repoIndex and returns the entity ID from
// moduleFQN that matches, regardless of kind prefix (py:func: or py:class:).
// This ensures imported classes resolve as imported_name_match alongside functions.
func pyMatchImport(moduleFQN, importedName string, repoIndex map[string][]string) (string, bool) {
	suffix := "." + importedName
	for _, id := range repoIndex[importedName] {
		// Accept any entity whose FQN ends with moduleFQN.importedName, ignoring the kind prefix.
		// Entity IDs follow the pattern "py:<kind>:<moduleFQN>.<name>".
		if strings.HasSuffix(id, moduleFQN+suffix) {
			return id, true
		}
	}
	return "", false
}

// processFile emits all entities, edges, and chunks for one Python file.
func (p pythonAnalyzer) processFile(
	relPath, moduleFQN string,
	src []byte, root *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	extImports []string,
	repoIndex map[string][]string,
	res *Result,
) {
	moduleID := "py:module:" + moduleFQN

	res.Entities = append(res.Entities, contract.Entity{
		ID:       moduleID,
		Name:     moduleFQN,
		Type:     contract.EntityPyModule,
		FilePath: relPath,
	})

	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: moduleID,
		Type:     contract.EntityPyModule,
		FilePath: relPath,
		Language: "python",
		Header:   fmt.Sprintf("[py_module] %s\nfile: %s", moduleFQN, relPath),
		Body:     string(src),
	})

	// Emit requires SymbolRows for unresolved external imports.
	for _, imp := range extImports {
		res.Symbols = append(res.Symbols, contract.SymbolRow{
			Symbol:   imp,
			Lang:     "python",
			Kind:     "module",
			Role:     contract.RoleRequires,
			EntityID: moduleID,
			SrcFile:  relPath,
		})
	}

	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "function_definition":
			p.emitFunc(relPath, moduleFQN, moduleID, "", false, src, child, moduleDefs, importMap, repoIndex, res)
			// provides SymbolRow for top-level function.
			if nameNode := child.ChildByFieldName("name"); nameNode != nil {
				fname := nameNode.Content(src)
				funcFQN := moduleFQN + "." + fname
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   funcFQN,
					Lang:     "python",
					Kind:     "func",
					Role:     contract.RoleProvides,
					EntityID: "py:func:" + funcFQN,
					SrcFile:  relPath,
				})
			}
		case "decorated_definition":
			inner := child.ChildByFieldName("definition")
			if inner != nil && inner.Type() == "function_definition" {
				p.emitFunc(relPath, moduleFQN, moduleID, "", true, src, inner, moduleDefs, importMap, repoIndex, res)
				// provides SymbolRow for decorated top-level function.
				if nameNode := inner.ChildByFieldName("name"); nameNode != nil {
					fname := nameNode.Content(src)
					funcFQN := moduleFQN + "." + fname
					res.Symbols = append(res.Symbols, contract.SymbolRow{
						Symbol:   funcFQN,
						Lang:     "python",
						Kind:     "func",
						Role:     contract.RoleProvides,
						EntityID: "py:func:" + funcFQN,
						SrcFile:  relPath,
					})
				}
			}
		case "class_definition":
			p.emitClass(relPath, moduleFQN, moduleID, src, child, moduleDefs, importMap, repoIndex, res)
			// provides SymbolRow for top-level class.
			if nameNode := child.ChildByFieldName("name"); nameNode != nil {
				cname := nameNode.Content(src)
				classFQN := moduleFQN + "." + cname
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   classFQN,
					Lang:     "python",
					Kind:     "class",
					Role:     contract.RoleProvides,
					EntityID: "py:class:" + classFQN,
					SrcFile:  relPath,
				})
			}
		}
	}
}

// pyExternalImports returns the module/package names from import statements that do not
// resolve to any known entity in the repo (i.e. unresolved external dependencies).
func pyExternalImports(root *sitter.Node, src []byte, repoIndex map[string][]string) []string {
	seen := map[string]bool{}
	var result []string
	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "import_statement":
			// import requests  or  import os.path  or  import requests as req
			nc := int(child.NamedChildCount())
			for j := 0; j < nc; j++ {
				n := child.NamedChild(j)
				if n == nil {
					continue
				}
				// For aliased_import ("import X as Y") use the real name field, not the whole content.
				var rawName string
				if n.Type() == "aliased_import" {
					nameField := n.ChildByFieldName("name")
					if nameField != nil {
						rawName = nameField.Content(src)
					} else {
						rawName = n.Content(src)
					}
				} else {
					rawName = n.Content(src)
				}
				// top-level module name (before first dot)
				topLevel := strings.SplitN(rawName, ".", 2)[0]
				if len(repoIndex[topLevel]) == 0 && !seen[topLevel] {
					seen[topLevel] = true
					result = append(result, topLevel)
				}
			}
		case "import_from_statement":
			// from flask import Flask -> module is "flask"
			modNode := child.ChildByFieldName("module_name")
			if modNode == nil {
				continue
			}
			modName := modNode.Content(src)
			// Relative imports (from . import x, from .helper import y) have a leading dot.
			// Their top-level split yields "" which must not be emitted as an external symbol.
			if strings.HasPrefix(modName, ".") || modNode.Type() == "relative_import" {
				continue
			}
			topLevel := strings.SplitN(modName, ".", 2)[0]
			if topLevel == "" {
				continue
			}
			// Check if any name imported from this module resolves in repoIndex.
			// If the module itself (top-level) has no repo entity, it's external.
			if len(repoIndex[topLevel]) == 0 && !seen[topLevel] {
				// Also make sure the imported names are not resolvable.
				// If all imported names are unresolvable, the whole module is external.
				allUnresolved := true
				nc := int(child.NamedChildCount())
				for j := 0; j < nc; j++ {
					n := child.NamedChild(j)
					if n == child.ChildByFieldName("module_name") {
						continue
					}
					// For aliased_import ("from pkg import a as b") use the real name,
					// not the whole "a as b" content, when looking up in repoIndex.
					var checkName string
					if n.Type() == "aliased_import" {
						nameField := n.ChildByFieldName("name")
						if nameField != nil {
							checkName = nameField.Content(src)
						} else {
							checkName = n.Content(src)
						}
					} else {
						checkName = n.Content(src)
					}
					if len(repoIndex[checkName]) > 0 {
						allUnresolved = false
						break
					}
				}
				if allUnresolved {
					seen[topLevel] = true
					result = append(result, topLevel)
				}
			}
		}
	}
	return result
}

// emitClass emits a py_class entity and its methods.
func (p pythonAnalyzer) emitClass(
	relPath, moduleFQN, moduleID string,
	src []byte, node *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	repoIndex map[string][]string,
	res *Result,
) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	cname := nameNode.Content(src)
	classID := "py:class:" + moduleFQN + "." + cname

	res.Entities = append(res.Entities, contract.Entity{
		ID:       classID,
		Name:     cname,
		Type:     contract.EntityPyClass,
		FilePath: relPath,
	})
	res.Edges = append(res.Edges, contract.Edge{
		From:     moduleID,
		To:       classID,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: classID,
		Type:     contract.EntityPyClass,
		FilePath: relPath,
		Language: "python",
		Header:   fmt.Sprintf("[py_class] %s.%s\nfile: %s", moduleFQN, cname, relPath),
		Body:     node.Content(src),
	})

	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}
	bc := int(body.NamedChildCount())
	for j := 0; j < bc; j++ {
		meth := body.NamedChild(j)
		if meth.Type() == "function_definition" {
			p.emitFunc(relPath, moduleFQN, classID, cname, false, src, meth, moduleDefs, importMap, repoIndex, res)
		}
	}
}

// emitFunc emits a py_func entity and its call edges.
// parentClass is empty for module-level functions.
// isDecorated indicates the function is wrapped in a decorated_definition node.
func (p pythonAnalyzer) emitFunc(
	relPath, moduleFQN, parentID, parentClass string,
	isDecorated bool,
	src []byte, node *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
	repoIndex map[string][]string,
	res *Result,
) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	fname := nameNode.Content(src)

	var funcFQN string
	if parentClass != "" {
		funcFQN = moduleFQN + "." + parentClass + "." + fname
	} else {
		funcFQN = moduleFQN + "." + fname
	}
	funcID := "py:func:" + funcFQN

	res.Entities = append(res.Entities, contract.Entity{
		ID:       funcID,
		Name:     fname,
		Type:     contract.EntityPyFunc,
		FilePath: relPath,
	})
	res.Edges = append(res.Edges, contract.Edge{
		From:     parentID,
		To:       funcID,
		Relation: contract.RelDefines,
		SrcFile:  relPath,
	})
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: funcID,
		Type:     contract.EntityPyFunc,
		FilePath: relPath,
		Language: "python",
		Header:   fmt.Sprintf("[py_func] %s\nfile: %s", funcFQN, relPath),
		Body:     node.Content(src),
	})

	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}

	danglingCalls := []string{}
	seenDangling := map[string]bool{}
	seenEdge := map[string]bool{}

	walkCalls(body, src, func(calleeNode *sitter.Node) {
		calleeName := calleeNode.Content(src)

		// Attribute call (e.g. obj.method) - treat as dangling.
		if calleeNode.Type() == "attribute" {
			if !seenDangling[calleeName] {
				seenDangling[calleeName] = true
				danglingCalls = append(danglingCalls, calleeName)
			}
			return
		}

		// Resolution ladder (plain identifier).
		//
		// Tier 1: scoped_name_match - callee defined in this module.
		if calleeID, ok := moduleDefs[calleeName]; ok {
			if seenEdge[funcID+"->"+calleeID] {
				return
			}
			seenEdge[funcID+"->"+calleeID] = true
			score, tier, props := edgeConfidence(contract.ResScopedNameMatch)
			if isDecorated {
				props = degraded(props, "decorator")
				score, tier = confidenceFromProps(props)
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:            funcID,
				To:              calleeID,
				Relation:        contract.RelCalls,
				SrcFile:         relPath,
				ConfidenceScore: score,
				ConfidenceTier:  tier,
				Properties:      props,
			})
			return
		}

		// Tier 2: imported_name_match - callee resolves through a tracked import.
		if calleeID, ok := importMap[calleeName]; ok {
			if seenEdge[funcID+"->"+calleeID] {
				return
			}
			seenEdge[funcID+"->"+calleeID] = true
			score, tier, props := edgeConfidence(contract.ResImportedNameMatch)
			if isDecorated {
				props = degraded(props, "decorator")
				score, tier = confidenceFromProps(props)
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:            funcID,
				To:              calleeID,
				Relation:        contract.RelCalls,
				SrcFile:         relPath,
				ConfidenceScore: score,
				ConfidenceTier:  tier,
				Properties:      props,
			})
			return
		}

		// Tier 3: global or ambiguous - look up in repo-wide index.
		ids := repoIndex[calleeName]
		switch len(ids) {
		case 0:
			// Tier 4: unresolved - dangling.
			if !seenDangling[calleeName] {
				seenDangling[calleeName] = true
				danglingCalls = append(danglingCalls, calleeName)
			}

		case 1:
			// global_name_match: unique repo-wide definition.
			if seenEdge[funcID+"->"+ids[0]] {
				return
			}
			seenEdge[funcID+"->"+ids[0]] = true
			score, tier, props := edgeConfidence(contract.ResGlobalNameMatch)
			if isDecorated {
				props = degraded(props, "decorator")
				score, tier = confidenceFromProps(props)
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:            funcID,
				To:              ids[0],
				Relation:        contract.RelCalls,
				SrcFile:         relPath,
				ConfidenceScore: score,
				ConfidenceTier:  tier,
				Properties:      props,
			})

		default:
			// ambiguous_multi_def: multiple repo-wide definitions.
			for _, id := range ids {
				if seenEdge[funcID+"->"+id] {
					continue
				}
				seenEdge[funcID+"->"+id] = true
				score, tier, props := edgeConfidence(contract.ResAmbiguousMultiDef)
				if isDecorated {
					props = degraded(props, "decorator")
					score, tier = confidenceFromProps(props)
				}
				res.Edges = append(res.Edges, contract.Edge{
					From:            funcID,
					To:              id,
					Relation:        contract.RelCalls,
					SrcFile:         relPath,
					ConfidenceScore: score,
					ConfidenceTier:  tier,
					Properties:      props,
				})
			}
		}
	})

	if len(danglingCalls) > 0 {
		for i, e := range res.Entities {
			if e.ID == funcID {
				if res.Entities[i].Properties == nil {
					res.Entities[i].Properties = map[string]string{}
				}
				res.Entities[i].Properties["dangling_call"] = strings.Join(danglingCalls, ",")
				break
			}
		}
	}
}

// degraded returns a copy of props with confidence capped at 0.45 (numeric) and
// degraded_by set (comma-joined if already present).
func degraded(props map[string]string, reason string) map[string]string {
	out := make(map[string]string, len(props)+1)
	for k, v := range props {
		out[k] = v
	}
	// Cap confidence at 0.45 using numeric comparison to avoid lexicographic ordering bugs.
	const cap = 0.45
	conf := out["confidence"]
	v, err := strconv.ParseFloat(conf, 64)
	if err != nil || v > cap {
		out["confidence"] = strconv.FormatFloat(cap, 'f', -1, 64)
	}
	// Append reason to degraded_by.
	if existing, ok := out["degraded_by"]; ok && existing != "" {
		out["degraded_by"] = existing + "," + reason
	} else {
		out["degraded_by"] = reason
	}
	return out
}

// walkCalls recursively walks a node tree and calls fn for every call
// expression's function child.
func walkCalls(node *sitter.Node, src []byte, fn func(*sitter.Node)) {
	if node == nil {
		return
	}
	if node.Type() == "call" {
		funcChild := node.ChildByFieldName("function")
		if funcChild != nil {
			fn(funcChild)
		}
	}
	count := int(node.ChildCount())
	for i := 0; i < count; i++ {
		walkCalls(node.Child(i), src, fn)
	}
}
