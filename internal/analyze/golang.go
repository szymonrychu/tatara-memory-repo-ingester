package analyze

import (
	"bufio"
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// goFuncInfo holds per-function data for call-edge and requires walking.
type goFuncInfo struct {
	entityID string
	decl     *ast.FuncDecl
	relFile  string
}

// goAnalyzer implements Analyzer for Go source files using go/packages + go/types.
type goAnalyzer struct {
	log             *slog.Logger
	crossRepoPrefix string
}

// NewGo returns an Analyzer for Go source files.
// crossRepoPrefix filters which external packages are emitted as requires symbols
// (e.g. "github.com/szymonrychu/").
func NewGo(crossRepoPrefix string) Analyzer {
	return goAnalyzer{log: slog.Default(), crossRepoPrefix: crossRepoPrefix}
}

// ClassifyRef reports whether a reference from objPkgPath (with object name
// objName) should be emitted as a requires SymbolRow given the current module
// path and the cross-repo prefix. Returns (emit, symbol).
// Exported so it can be unit-tested directly without a full packages.Load fixture.
func ClassifyRef(objPkgPath, objName, modulePath, prefix string) (bool, string) {
	// In-module reference: never emit.
	if strings.HasPrefix(objPkgPath, modulePath) {
		return false, ""
	}
	// External but not under the prefix: skip (stdlib, third-party not ours).
	if !strings.HasPrefix(objPkgPath, prefix) {
		return false, ""
	}
	return true, objPkgPath + "." + objName
}

// Name returns the analyzer name.
func (g goAnalyzer) Name() string { return "go" }

// Match reports whether the path is a Go source file.
func (g goAnalyzer) Match(path string) bool {
	return strings.HasSuffix(path, ".go")
}

// Analyze loads all packages under repoRoot and emits entities, edges, and chunks.
// Only entities whose FilePath is in the files set are emitted.
func (g goAnalyzer) Analyze(ctx context.Context, repoRoot string, files []string) (Result, error) {
	absRepoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("filepath.Abs(%q): %w", repoRoot, err)
	}
	// Resolve symlinks so that filepath.Rel works correctly on macOS where /tmp -> /private/tmp.
	if resolved, err2 := filepath.EvalSymlinks(absRepoRoot); err2 == nil {
		absRepoRoot = resolved
	}

	// Build scope set from caller-supplied file list (repo-relative paths).
	scope := make(map[string]bool, len(files))
	for _, f := range files {
		scope[f] = true
	}

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

	modulePath := readModulePath(repoRoot)

	var res Result

	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			g.log.Warn("package has errors, using tree-sitter fallback",
				slog.String("pkg", pkg.PkgPath),
				slog.Int("errors", len(pkg.Errors)),
			)
			// Collect absolute paths of in-scope files for this package.
			var pkgFiles []string
			for _, goFile := range pkg.GoFiles {
				rel := repoRelPath(absRepoRoot, goFile)
				if scope[rel] {
					pkgFiles = append(pkgFiles, goFile)
				}
			}
			if len(pkgFiles) == 0 {
				continue
			}
			fallbackRes := fallbackAnalyzeGoPackage(g.log, modulePath, absRepoRoot, pkgFiles)
			res.Entities = append(res.Entities, fallbackRes.Entities...)
			res.Edges = append(res.Edges, fallbackRes.Edges...)
			res.Chunks = append(res.Chunks, fallbackRes.Chunks...)
			res.Symbols = append(res.Symbols, fallbackRes.Symbols...)
			res.ParseErrors += fallbackRes.ParseErrors
			continue
		}
		g.processPackage(pkg, pkgPaths, absRepoRoot, scope, modulePath, &res)
	}

	return res, nil
}

// readModulePath reads the module path from go.mod in repoRoot.
func readModulePath(repoRoot string) string {
	f, err := os.Open(filepath.Join(repoRoot, "go.mod")) //nolint:gosec
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimPrefix(line, "module ")
		}
	}
	return ""
}

func (g goAnalyzer) processPackage(
	pkg *packages.Package,
	pkgPaths map[string]bool,
	absRepoRoot string,
	scope map[string]bool,
	modulePath string,
	res *Result,
) {
	pkgID := "go:package:" + pkg.PkgPath

	// pkgEntityEmitted tracks whether we have already appended the package entity.
	// We defer emission until the first in-scope func/method is found so that
	// packages with no in-scope files contribute nothing to the push payload.
	pkgEntityEmitted := false
	emitPkgEntityOnce := func() {
		if pkgEntityEmitted {
			return
		}
		pkgEntityEmitted = true
		res.Entities = append(res.Entities, contract.Entity{
			ID:   pkgID,
			Name: pkg.Name,
			Type: contract.EntityGoPackage,
		})
	}

	// fileCache avoids re-reading the same source file for every func it contains.
	fileCache := make(map[string][]byte)
	cachedReadFile := func(absFilePath string) []byte {
		if b, ok := fileCache[absFilePath]; ok {
			return b
		}
		b, err := osReadFile(absFilePath) //nolint:gosec
		if err != nil {
			b = nil
		}
		fileCache[absFilePath] = b
		return b
	}

	var funcs []goFuncInfo

	for _, file := range pkg.Syntax {
		absFilePath := pkg.Fset.File(file.Pos()).Name()
		rel := repoRelPath(absRepoRoot, absFilePath)

		// Skip files not in scope.
		if !scope[rel] {
			continue
		}

		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			emitPkgEntityOnce()

			entityID, entityType, name := funcEntityID(pkg.PkgPath, fd)

			// Determine signature string.
			sig := funcSignature(pkg.Fset, pkg.TypesInfo, fd)

			lineStart := pkg.Fset.Position(fd.Pos()).Line
			lineEnd := pkg.Fset.Position(fd.End()).Line
			props := map[string]string{
				"line_start": fmt.Sprintf("%d", lineStart),
				"line_end":   fmt.Sprintf("%d", lineEnd),
				"signature":  sig,
				"exported":   fmt.Sprintf("%v", ast.IsExported(fd.Name.Name)),
			}

			res.Entities = append(res.Entities, contract.Entity{
				ID:         entityID,
				Name:       name,
				Type:       entityType,
				FilePath:   rel,
				LineStart:  lineStart,
				LineEnd:    lineEnd,
				Properties: props,
			})

			// provides SymbolRow for exported package-level declarations.
			if ast.IsExported(fd.Name.Name) {
				kind := goEntityKind(entityType)
				symbol := pkg.PkgPath + "." + fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					recv := receiverTypeName(fd.Recv.List[0].Type)
					symbol = pkg.PkgPath + "." + recv + "." + fd.Name.Name
				}
				res.Symbols = append(res.Symbols, contract.SymbolRow{
					Symbol:   symbol,
					Lang:     "go",
					Kind:     kind,
					Role:     contract.RoleProvides,
					EntityID: entityID,
					SrcFile:  rel,
				})
			}

			// defines edge: package -> func/method
			res.Edges = append(res.Edges, contract.Edge{
				From:     pkgID,
				To:       entityID,
				Relation: contract.RelDefines,
				SrcFile:  rel,
			})

			// Chunk: slice from cached bytes to avoid redundant disk reads.
			src := cachedReadFile(absFilePath)
			body := sourceSliceBytes(pkg.Fset, fd.Pos(), fd.End(), src)
			header := fmt.Sprintf("[%s] %s\nfile: %s\npackage: %s\nsignature: %s",
				entityType, entityID, rel, pkg.PkgPath, sig)
			res.Chunks = append(res.Chunks, contract.Chunk{
				EntityID: entityID,
				Type:     entityType,
				FilePath: rel,
				Language: "go",
				Header:   header,
				Body:     body,
			})

			funcs = append(funcs, goFuncInfo{entityID: entityID, decl: fd, relFile: rel})
		}
	}

	// Walk each function body for call edges.
	for _, fi := range funcs {
		if fi.decl.Body == nil {
			continue
		}
		g.emitCallEdges(pkg, fi.entityID, fi.relFile, fi.decl.Body, pkgPaths, scope, res)
	}

	// Emit requires SymbolRows: walk TypesInfo.Uses for cross-module refs under crossRepoPrefix.
	// Iterate in Pos() order for reproducible output (map iteration order is random).
	if g.crossRepoPrefix != "" && modulePath != "" && pkg.TypesInfo != nil {
		type usesEntry struct {
			ident *ast.Ident
			obj   types.Object
		}
		sorted := make([]usesEntry, 0, len(pkg.TypesInfo.Uses))
		for ident, obj := range pkg.TypesInfo.Uses {
			sorted = append(sorted, usesEntry{ident, obj})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].ident.Pos() < sorted[j].ident.Pos()
		})

		seenReq := map[string]bool{}
		for _, entry := range sorted {
			ident, obj := entry.ident, entry.obj
			if obj == nil || obj.Pkg() == nil {
				continue
			}
			objPkgPath := obj.Pkg().Path()
			// For methods, reconstruct the receiver-qualified name so the requires key
			// matches the provides key emitted by the processPackage provides side:
			// "<pkgPath>.<RecvType>.<Method>".  For funcs/types, obj.Name() is correct.
			objName := crossRepoSymbolName(obj)
			emit, symbol := ClassifyRef(objPkgPath, objName, modulePath, g.crossRepoPrefix)
			if !emit {
				continue
			}
			// Find which in-scope func contains this ident.
			pos := pkg.Fset.Position(ident.Pos())
			absFile := pos.Filename
			rel := repoRelPath(absRepoRoot, absFile)
			if !scope[rel] {
				continue
			}
			// Find the containing entity (func) for this ident.
			entityID := g.containingEntity(pkg, ident.Pos(), funcs)
			key := symbol + "|" + entityID
			if seenReq[key] {
				continue
			}
			seenReq[key] = true
			res.Symbols = append(res.Symbols, contract.SymbolRow{
				Symbol:   symbol,
				Lang:     "go",
				Kind:     goObjKind(obj),
				Role:     contract.RoleRequires,
				EntityID: entityID,
				SrcFile:  rel,
			})
		}
	}
}

// containingEntity finds the entity ID of the func/method in funcs that contains pos.
func (g goAnalyzer) containingEntity(pkg *packages.Package, pos token.Pos, funcs []goFuncInfo) string {
	for _, fi := range funcs {
		if fi.decl.Pos() <= pos && pos <= fi.decl.End() {
			return fi.entityID
		}
	}
	// Fall back to package entity.
	return "go:package:" + pkg.PkgPath
}

// goEntityKind maps an entity type constant to a symbol kind string.
func goEntityKind(entityType string) string {
	switch entityType {
	case contract.EntityGoMethod:
		return "method"
	case contract.EntityGoType:
		return "type"
	default:
		return "func"
	}
}

// goObjKind returns a symbol kind string for a types.Object.
func goObjKind(obj types.Object) string {
	switch v := obj.(type) {
	case *types.TypeName:
		return "type"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	case *types.Func:
		if sig, ok := v.Type().(*types.Signature); ok && sig.Recv() != nil {
			return "method"
		}
		return "func"
	default:
		return "func"
	}
}

// crossRepoSymbolName returns the name component for a cross-repo requires SymbolRow.
// For methods it returns "<RecvTypeName>.<MethodName>" so the key matches the provides side
// which emits "<pkgPath>.<RecvTypeName>.<MethodName>".
// For all other objects it returns obj.Name().
func crossRepoSymbolName(obj types.Object) string {
	fn, ok := obj.(*types.Func)
	if !ok {
		return obj.Name()
	}
	sig, ok2 := fn.Type().(*types.Signature)
	if !ok2 || sig.Recv() == nil {
		return obj.Name()
	}
	recv := sig.Recv().Type()
	if ptr, isPtr := recv.(*types.Pointer); isPtr {
		recv = ptr.Elem()
	}
	if named, isNamed := recv.(*types.Named); isNamed {
		return named.Obj().Name() + "." + fn.Name()
	}
	return obj.Name()
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
// callerRelFile is the repo-relative path of the file containing the caller; it
// is already known to be in scope.
func (g goAnalyzer) emitCallEdges(
	pkg *packages.Package,
	callerID string,
	callerRelFile string,
	body *ast.BlockStmt,
	pkgPaths map[string]bool,
	_ map[string]bool,
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

		score := scoreFor(contract.ResTypeResolved)
		res.Edges = append(res.Edges, contract.Edge{
			From:            callerID,
			To:              calleeID,
			Relation:        contract.RelCalls,
			SrcFile:         callerRelFile,
			ConfidenceScore: score,
			ConfidenceTier:  contract.TierForScore(score),
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

// repoRelPath returns the path of absFilePath relative to absRepoRoot.
// Falls back to absFilePath if Rel fails (should not happen in practice).
func repoRelPath(absRepoRoot, absFilePath string) string {
	rel, err := filepath.Rel(absRepoRoot, absFilePath)
	if err != nil {
		return absFilePath
	}
	return rel
}

// osReadFile is the os.ReadFile implementation used by this package.
// Tests may replace it to intercept reads.
var osReadFile = os.ReadFile //nolint:gosec

// sourceSliceBytes returns the source substring [start,end) from pre-loaded bytes.
func sourceSliceBytes(fset *token.FileSet, start, end token.Pos, src []byte) string {
	startOff := fset.Position(start).Offset
	endOff := fset.Position(end).Offset
	if startOff < 0 || endOff > len(src) || startOff >= endOff {
		return string(src)
	}
	return string(src[startOff:endOff])
}

// scoreFor parses the confidence prior string for a resolution level into a float.
func scoreFor(resolution string) float64 {
	switch resolution {
	case contract.ResTypeResolved:
		return 0.98
	case contract.ResScopedNameMatch:
		return 0.85
	case contract.ResImportedNameMatch:
		return 0.7
	case contract.ResGlobalNameMatch:
		return 0.45
	case contract.ResAmbiguousMultiDef:
		return 0.2
	default:
		return 0.0
	}
}
