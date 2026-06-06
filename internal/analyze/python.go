package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
func (p pythonAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	lang := python.GetLanguage()

	// First pass: parse every file and collect per-file AST + src for the repo-wide index.
	type parsedFile struct {
		relPath   string
		moduleFQN string
		src       []byte
		root      *sitter.Node
	}
	parsed := make([]parsedFile, 0, len(files))
	for _, relPath := range files {
		absPath := filepath.Join(repoRoot, relPath)
		src, err := os.ReadFile(absPath) //nolint:gosec
		if err != nil {
			return Result{}, fmt.Errorf("read %q: %w", absPath, err)
		}
		root, err := sitter.ParseCtx(context.Background(), src, lang)
		if err != nil {
			p.log.Warn("tree-sitter parse error", slog.String("file", relPath), slog.String("err", err.Error()))
			continue
		}
		parsed = append(parsed, parsedFile{
			relPath:   relPath,
			moduleFQN: pathToFQN(relPath),
			src:       src,
			root:      root,
		})
	}

	// Build repo-wide name -> []entityID index from all parsed files.
	// Used for global_name_match and ambiguous_multi_def resolution.
	repoIndex := map[string][]string{}
	for _, pf := range parsed {
		defs := pyFileDefs(pf.moduleFQN, pf.root, pf.src)
		for name, id := range defs {
			repoIndex[name] = append(repoIndex[name], id)
		}
	}

	var res Result

	for _, pf := range parsed {
		// Build module-level def map (scoped resolution).
		moduleDefs := pyFileDefs(pf.moduleFQN, pf.root, pf.src)

		// Build import map: local name -> entityID resolved via tracked imports.
		importMap := pyImportMap(pf.root, pf.src, repoIndex)

		p.processFile(pf.relPath, pf.moduleFQN, pf.src, pf.root, moduleDefs, importMap, repoIndex, &res)
	}

	return res, nil
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
			// Convert "pkg.helper" -> "pkg.helper"
			moduleFQN := moduleName

			nc := int(child.NamedChildCount())
			for j := 0; j < nc; j++ {
				n := child.NamedChild(j)
				if n == child.ChildByFieldName("module_name") {
					continue
				}
				// Each imported name is a dotted_name or identifier node.
				localName := n.Content(src)
				candidateID := "py:func:" + moduleFQN + "." + localName
				// Check if this entity is known in the repo index.
				ids := repoIndex[localName]
				for _, id := range ids {
					if id == candidateID {
						imports[localName] = candidateID
						break
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

// processFile emits all entities, edges, and chunks for one Python file.
func (p pythonAnalyzer) processFile(
	relPath, moduleFQN string,
	src []byte, root *sitter.Node,
	moduleDefs map[string]string,
	importMap map[string]string,
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

	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "function_definition":
			p.emitFunc(relPath, moduleFQN, moduleID, "", false, src, child, moduleDefs, importMap, repoIndex, res)
		case "decorated_definition":
			inner := child.ChildByFieldName("definition")
			if inner != nil && inner.Type() == "function_definition" {
				p.emitFunc(relPath, moduleFQN, moduleID, "", true, src, inner, moduleDefs, importMap, repoIndex, res)
			}
		case "class_definition":
			p.emitClass(relPath, moduleFQN, moduleID, src, child, moduleDefs, importMap, repoIndex, res)
		}
	}
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

	walkCalls(body, src, func(calleeNode *sitter.Node) {
		calleeName := calleeNode.Content(src)

		// Attribute call (e.g. obj.method) - treat as dangling.
		if calleeNode.Type() == "attribute" {
			danglingCalls = append(danglingCalls, calleeName)
			return
		}

		// Resolution ladder (plain identifier).
		//
		// Tier 1: scoped_name_match - callee defined in this module.
		if calleeID, ok := moduleDefs[calleeName]; ok {
			conf := contract.ConfidenceFor(contract.ResScopedNameMatch)
			props := map[string]string{
				"resolution": contract.ResScopedNameMatch,
				"confidence": conf,
			}
			if isDecorated {
				props = degraded(props, "decorator")
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:       funcID,
				To:         calleeID,
				Relation:   contract.RelCalls,
				SrcFile:    relPath,
				Properties: props,
			})
			return
		}

		// Tier 2: imported_name_match - callee resolves through a tracked import.
		if calleeID, ok := importMap[calleeName]; ok {
			conf := contract.ConfidenceFor(contract.ResImportedNameMatch)
			props := map[string]string{
				"resolution": contract.ResImportedNameMatch,
				"confidence": conf,
			}
			if isDecorated {
				props = degraded(props, "decorator")
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:       funcID,
				To:         calleeID,
				Relation:   contract.RelCalls,
				SrcFile:    relPath,
				Properties: props,
			})
			return
		}

		// Tier 3: global or ambiguous - look up in repo-wide index.
		ids := repoIndex[calleeName]
		switch len(ids) {
		case 0:
			// Tier 4: unresolved - dangling.
			danglingCalls = append(danglingCalls, calleeName)

		case 1:
			// global_name_match: unique repo-wide definition.
			conf := contract.ConfidenceFor(contract.ResGlobalNameMatch)
			props := map[string]string{
				"resolution": contract.ResGlobalNameMatch,
				"confidence": conf,
			}
			if isDecorated {
				props = degraded(props, "decorator")
			}
			res.Edges = append(res.Edges, contract.Edge{
				From:       funcID,
				To:         ids[0],
				Relation:   contract.RelCalls,
				SrcFile:    relPath,
				Properties: props,
			})

		default:
			// ambiguous_multi_def: multiple repo-wide definitions.
			for _, id := range ids {
				props := map[string]string{
					"resolution": contract.ResAmbiguousMultiDef,
					"confidence": contract.ConfidenceFor(contract.ResAmbiguousMultiDef),
				}
				if isDecorated {
					props = degraded(props, "decorator")
				}
				res.Edges = append(res.Edges, contract.Edge{
					From:       funcID,
					To:         id,
					Relation:   contract.RelCalls,
					SrcFile:    relPath,
					Properties: props,
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

// degraded returns a copy of props with confidence capped at "0.45" and
// degraded_by set (comma-joined if already present).
func degraded(props map[string]string, reason string) map[string]string {
	out := make(map[string]string, len(props)+1)
	for k, v := range props {
		out[k] = v
	}
	// Cap confidence at 0.45.
	conf := out["confidence"]
	if conf == "" || conf > "0.45" {
		out["confidence"] = "0.45"
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
