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
	// Build scope set.
	scope := make(map[string]bool, len(files))
	for _, f := range files {
		scope[f] = true
	}

	lang := python.GetLanguage()
	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	var res Result

	for _, relPath := range files {
		if !scope[relPath] {
			continue
		}
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

		moduleFQN := pathToFQN(relPath)
		p.processFile(relPath, moduleFQN, src, root, &res)
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

// processFile emits all entities, edges, and chunks for one Python file.
func (p pythonAnalyzer) processFile(relPath, moduleFQN string, src []byte, root *sitter.Node, res *Result) {
	moduleID := "py:module:" + moduleFQN

	res.Entities = append(res.Entities, contract.Entity{
		ID:       moduleID,
		Name:     moduleFQN,
		Type:     contract.EntityPyModule,
		FilePath: relPath,
	})

	// Chunk for the module.
	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: moduleID,
		Type:     contract.EntityPyModule,
		FilePath: relPath,
		Language: "python",
		Header:   fmt.Sprintf("[py_module] %s\nfile: %s", moduleFQN, relPath),
		Body:     string(src),
	})

	// Build module-level def map for scoped call resolution.
	moduleDefs := pyFileDefs(moduleFQN, root, src)

	count := int(root.NamedChildCount())
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "function_definition":
			p.emitFunc(relPath, moduleFQN, moduleID, "", src, child, moduleDefs, res)
		case "class_definition":
			p.emitClass(relPath, moduleFQN, moduleID, src, child, moduleDefs, res)
		}
	}
}

// emitClass emits a py_class entity and its methods.
func (p pythonAnalyzer) emitClass(
	relPath, moduleFQN, moduleID string,
	src []byte, node *sitter.Node,
	moduleDefs map[string]string,
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
			p.emitFunc(relPath, moduleFQN, classID, cname, src, meth, moduleDefs, res)
		}
	}
}

// emitFunc emits a py_func entity and its call edges.
// parentClass is empty for module-level functions.
func (p pythonAnalyzer) emitFunc(
	relPath, moduleFQN, parentID, parentClass string,
	src []byte, node *sitter.Node,
	moduleDefs map[string]string,
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

	// Walk the function body for calls.
	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}

	// Track dangling calls per entity (appended as comma-joined property).
	danglingCalls := []string{}

	walkCalls(body, src, func(calleeNode *sitter.Node) {
		calleeName := calleeNode.Content(src)

		// Attribute call (e.g. obj.method) - callee is the selector, we only have the
		// attribute name; treat as dangling unless it resolves to a module def.
		if calleeNode.Type() == "attribute" {
			danglingCalls = append(danglingCalls, calleeName)
			return
		}

		// Plain identifier call.
		if calleeID, ok := moduleDefs[calleeName]; ok {
			// scoped_name_match: defined in this module
			res.Edges = append(res.Edges, contract.Edge{
				From:     funcID,
				To:       calleeID,
				Relation: contract.RelCalls,
				SrcFile:  relPath,
				Properties: map[string]string{
					"resolution": contract.ResScopedNameMatch,
					"confidence": contract.ConfidenceFor(contract.ResScopedNameMatch),
				},
			})
			return
		}

		// Unresolved (builtin / external) - record as dangling, no edge.
		danglingCalls = append(danglingCalls, calleeName)
	})

	if len(danglingCalls) > 0 {
		// Attach dangling_call property to the entity.
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
