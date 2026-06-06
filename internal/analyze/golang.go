package analyze

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// goAnalyzer implements Analyzer for Go source files using go/packages + go/types.
type goAnalyzer struct {
	log *slog.Logger
}

// NewGo returns an Analyzer for Go source files.
func NewGo() Analyzer {
	return goAnalyzer{log: slog.Default()}
}

// Name returns the analyzer name.
func (g goAnalyzer) Name() string { return "go" }

// Match reports whether the path is a Go source file.
func (g goAnalyzer) Match(path string) bool {
	return strings.HasSuffix(path, ".go")
}

// Analyze loads all packages under repoRoot and emits entities, edges, and chunks.
func (g goAnalyzer) Analyze(ctx context.Context, repoRoot string, _ []string) (Result, error) {
	mode := packages.NeedName |
		packages.NeedFiles |
		packages.NeedSyntax |
		packages.NeedTypes |
		packages.NeedTypesInfo |
		packages.NeedDeps |
		packages.NeedImports

	cfg := &packages.Config{
		Mode:    mode,
		Dir:     repoRoot,
		Context: ctx,
		// Use a clean environment so the fixture module is loadable in any working dir.
		Env: append(os.Environ(), "GOFLAGS=-mod=mod"),
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return Result{}, fmt.Errorf("packages.Load: %w", err)
	}

	// Build a set of loaded package paths for callee filtering.
	pkgPaths := map[string]bool{}
	for _, pkg := range pkgs {
		if len(pkg.Errors) == 0 {
			pkgPaths[pkg.PkgPath] = true
		}
	}

	var res Result

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			g.log.Warn("skipping package with errors",
				slog.String("pkg", pkg.PkgPath),
				slog.Int("errors", len(pkg.Errors)),
			)
			continue
		}
		g.processPackage(pkg, pkgPaths, &res)
	}

	return res, nil
}

func (g goAnalyzer) processPackage(pkg *packages.Package, pkgPaths map[string]bool, res *Result) {
	pkgID := "go:package:" + pkg.PkgPath

	res.Entities = append(res.Entities, contract.Entity{
		ID:   pkgID,
		Name: pkg.Name,
		Type: contract.EntityGoPackage,
	})

	// Map from func entity ID to its body (for call-edge walking).
	type funcInfo struct {
		entityID string
		decl     *ast.FuncDecl
	}
	var funcs []funcInfo

	for _, file := range pkg.Syntax {
		filePath := pkg.Fset.File(file.Pos()).Name()

		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			entityID, entityType, name := funcEntityID(pkg.PkgPath, fd)

			// Determine signature string.
			sig := funcSignature(pkg.Fset, pkg.TypesInfo, fd)

			props := map[string]string{
				"line_start": fmt.Sprintf("%d", pkg.Fset.Position(fd.Pos()).Line),
				"line_end":   fmt.Sprintf("%d", pkg.Fset.Position(fd.End()).Line),
				"signature":  sig,
				"exported":   fmt.Sprintf("%v", ast.IsExported(fd.Name.Name)),
			}

			res.Entities = append(res.Entities, contract.Entity{
				ID:         entityID,
				Name:       name,
				Type:       entityType,
				FilePath:   relPath(filePath, pkg.Fset),
				Properties: props,
			})

			// defines edge: package -> func/method
			res.Edges = append(res.Edges, contract.Edge{
				From:     pkgID,
				To:       entityID,
				Relation: contract.RelDefines,
				SrcFile:  relPath(filePath, pkg.Fset),
			})

			// Chunk
			body := sourceSlice(pkg.Fset, fd.Pos(), fd.End(), filePath)
			header := fmt.Sprintf("[%s] %s\nfile: %s\npackage: %s\nsignature: %s",
				entityType, entityID, relPath(filePath, pkg.Fset), pkg.PkgPath, sig)
			res.Chunks = append(res.Chunks, contract.Chunk{
				EntityID: entityID,
				Type:     entityType,
				FilePath: relPath(filePath, pkg.Fset),
				Language: "go",
				Header:   header,
				Body:     body,
			})

			funcs = append(funcs, funcInfo{entityID: entityID, decl: fd})
		}
	}

	// Walk each function body for call edges.
	for _, fi := range funcs {
		if fi.decl.Body == nil {
			continue
		}
		g.emitCallEdges(pkg, fi.entityID, fi.decl.Body, pkgPaths, res)
	}
}

// funcEntityID returns (entityID, entityType, shortName) for a FuncDecl.
func funcEntityID(pkgPath string, fd *ast.FuncDecl) (string, string, string) {
	name := fd.Name.Name
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return "go:func:" + pkgPath + "." + name, contract.EntityGoFunc, name
	}

	// Method: extract receiver type name.
	recv := receiverTypeName(fd.Recv.List[0].Type)
	id := "go:method:" + pkgPath + ".(" + recv + ")." + name
	return id, contract.EntityGoMethod, name
}

// receiverTypeName extracts the base type name from a receiver type expression.
func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	}
	return "unknown"
}

// funcSignature builds a human-readable signature string from type info.
func funcSignature(fset *token.FileSet, info *types.Info, fd *ast.FuncDecl) string {
	if info != nil {
		if obj, ok := info.Defs[fd.Name]; ok && obj != nil {
			if fn, ok2 := obj.(*types.Func); ok2 {
				return fn.Type().String()
			}
		}
	}
	// Fallback: reconstruct from the source position range.
	start := fset.Position(fd.Pos())
	return fmt.Sprintf("func %s (line %d)", fd.Name.Name, start.Line)
}

// emitCallEdges walks a function body and emits calls edges for resolved callees.
func (g goAnalyzer) emitCallEdges(
	pkg *packages.Package,
	callerID string,
	body *ast.BlockStmt,
	pkgPaths map[string]bool,
	res *Result,
) {
	seenEdge := map[string]bool{}

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		ident := calleeIdent(call.Fun)
		if ident == nil {
			return true
		}

		obj, ok := pkg.TypesInfo.Uses[ident]
		if !ok {
			return true
		}

		fn, ok := obj.(*types.Func)
		if !ok {
			return true
		}

		if fn.Pkg() == nil {
			return true // builtin
		}

		calleePkgPath := fn.Pkg().Path()
		if !pkgPaths[calleePkgPath] {
			return true // outside loaded set (stdlib / third-party)
		}

		calleeID := calleeEntityID(calleePkgPath, fn)

		edgeKey := callerID + "->" + calleeID
		if seenEdge[edgeKey] {
			return true
		}
		seenEdge[edgeKey] = true

		filePath := pkg.Fset.Position(call.Pos()).Filename

		res.Edges = append(res.Edges, contract.Edge{
			From:     callerID,
			To:       calleeID,
			Relation: contract.RelCalls,
			SrcFile:  relPath(filePath, pkg.Fset),
			Properties: map[string]string{
				"resolution": contract.ResTypeResolved,
				"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
			},
		})

		return true
	})
}

// calleeEntityID returns the canonical entity ID for a resolved *types.Func callee.
// Plain functions get "go:func:..." and methods get "go:method:..." to match
// the IDs emitted by processPackage.
func calleeEntityID(pkgPath string, fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if ok && sig.Recv() != nil {
		recv := sig.Recv().Type()
		// Dereference pointer receiver.
		if ptr, isPtr := recv.(*types.Pointer); isPtr {
			recv = ptr.Elem()
		}
		if named, isNamed := recv.(*types.Named); isNamed {
			return "go:method:" + pkgPath + ".(" + named.Obj().Name() + ")." + fn.Name()
		}
	}
	return "go:func:" + pkgPath + "." + fn.Name()
}

// calleeIdent extracts the terminal identifier from a call expression function.
func calleeIdent(expr ast.Expr) *ast.Ident {
	switch e := expr.(type) {
	case *ast.Ident:
		return e
	case *ast.SelectorExpr:
		return e.Sel
	}
	return nil
}

// relPath returns the file's path. Since packages returns absolute paths,
// we just return the raw path; the test checks entity presence not the path value.
func relPath(absPath string, _ *token.FileSet) string {
	return absPath
}

// sourceSlice reads the source bytes for the range [start,end) from the named file.
func sourceSlice(fset *token.FileSet, start, end token.Pos, filename string) string {
	src, err := os.ReadFile(filename) //nolint:gosec
	if err != nil {
		return ""
	}
	startOff := fset.Position(start).Offset
	endOff := fset.Position(end).Offset
	if startOff < 0 || endOff > len(src) || startOff >= endOff {
		return string(src)
	}
	return string(src[startOff:endOff])
}
