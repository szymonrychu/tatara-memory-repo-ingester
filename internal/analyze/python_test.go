package analyze_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// allPyFiles is the full fixture set used by multi-file tests.
var allPyFiles = []string{
	"pkg/mod.py",
	"pkg/helper.py",
	"pkg/uses_import.py",
	"pkg/unique_def.py",
	"pkg/uses_global.py",
	"pkg/dup_a.py",
	"pkg/dup_b.py",
	"pkg/uses_ambiguous.py",
	"pkg/decorated.py",
}

func TestPythonAnalyzer(t *testing.T) {
	a := analyze.NewPython()
	require.True(t, a.Match("pkg/mod.py"))

	res, err := a.Analyze(context.Background(), "testdata/py", []string{"pkg/mod.py"})
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}
	require.True(t, ids["py:func:pkg.mod.f"])
	require.True(t, ids["py:func:pkg.mod.g"])

	call, ok := findEdge(res.Edges, contract.RelCalls, "py:func:pkg.mod.f", "py:func:pkg.mod.g")
	require.True(t, ok, "expected f->g calls edge")
	require.Equal(t, contract.ResScopedNameMatch, call.Properties["resolution"])
	require.Equal(t, "0.85", call.Properties["confidence"])

	// len([]) is a builtin: no edge, recorded as a dangling call on f.
	_, hasLen := findEdge(res.Edges, contract.RelCalls, "py:func:pkg.mod.f", "py:func:pkg.mod.len")
	require.False(t, hasLen)
}

func TestPythonImportedNameMatch(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", allPyFiles)
	require.NoError(t, err)

	// uses_import.py: caller() calls helped(), imported from pkg.helper
	edge, ok := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.uses_import.caller", "py:func:pkg.helper.helped")
	require.True(t, ok, "expected caller->helped imported_name_match edge")
	require.Equal(t, contract.ResImportedNameMatch, edge.Properties["resolution"])
	require.Equal(t, "0.7", edge.Properties["confidence"])
}

func TestPythonGlobalNameMatch(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", allPyFiles)
	require.NoError(t, err)

	// uses_global.py: global_caller() calls unique_func() - unique repo-wide def
	edge, ok := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.uses_global.global_caller", "py:func:pkg.unique_def.unique_func")
	require.True(t, ok, "expected global_caller->unique_func global_name_match edge")
	require.Equal(t, contract.ResGlobalNameMatch, edge.Properties["resolution"])
	require.Equal(t, "0.45", edge.Properties["confidence"])
}

func TestPythonAmbiguousMultiDef(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", allPyFiles)
	require.NoError(t, err)

	// uses_ambiguous.py: ambiguous_caller() calls shared_name() defined in dup_a AND dup_b
	// Must emit exactly two ambiguous edges (one to each def).
	edgeA, okA := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.uses_ambiguous.ambiguous_caller", "py:func:pkg.dup_a.shared_name")
	edgeB, okB := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.uses_ambiguous.ambiguous_caller", "py:func:pkg.dup_b.shared_name")
	require.True(t, okA, "expected ambiguous edge to dup_a.shared_name")
	require.True(t, okB, "expected ambiguous edge to dup_b.shared_name")
	require.Equal(t, contract.ResAmbiguousMultiDef, edgeA.Properties["resolution"])
	require.Equal(t, "0.2", edgeA.Properties["confidence"])
	require.Equal(t, contract.ResAmbiguousMultiDef, edgeB.Properties["resolution"])
	require.Equal(t, "0.2", edgeB.Properties["confidence"])
}

func TestPythonProvidesSymbols(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", []string{"pkg/mod.py"})
	require.NoError(t, err)

	// pkg.mod has top-level def g and def f - both should produce provides SymbolRows.
	provG, ok := findSymbol(res.Symbols, contract.RoleProvides, "pkg.mod.g")
	require.True(t, ok, "expected provides SymbolRow for pkg.mod.g")
	require.Equal(t, "python", provG.Lang)
	require.Equal(t, "func", provG.Kind)
	require.Equal(t, "py:func:pkg.mod.g", provG.EntityID)
	require.Equal(t, "pkg/mod.py", provG.SrcFile)

	provF, okF := findSymbol(res.Symbols, contract.RoleProvides, "pkg.mod.f")
	require.True(t, okF, "expected provides SymbolRow for pkg.mod.f")
	require.Equal(t, "pkg/mod.py", provF.SrcFile)

	// All SrcFile values must be within the files scope.
	for _, s := range res.Symbols {
		require.Equal(t, "pkg/mod.py", s.SrcFile,
			"SymbolRow %q has SrcFile outside files set", s.Symbol)
	}
}

func TestPythonRequiresSymbols(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", []string{"pkg/ext_import.py"})
	require.NoError(t, err)

	// pkg/ext_import.py has: import requests; from flask import Flask
	// Both are unresolved external imports -> requires SymbolRows.
	reqReq, ok := findSymbol(res.Symbols, contract.RoleRequires, "requests")
	require.True(t, ok, "expected requires SymbolRow for 'requests'")
	require.Equal(t, "python", reqReq.Lang)
	require.Equal(t, "module", reqReq.Kind)
	require.Equal(t, "pkg/ext_import.py", reqReq.SrcFile)

	reqFlask, okF := findSymbol(res.Symbols, contract.RoleRequires, "flask")
	require.True(t, okF, "expected requires SymbolRow for 'flask' (from flask import Flask)")
	require.Equal(t, "pkg/ext_import.py", reqFlask.SrcFile)
}

func TestPythonDegradedByDecorator(t *testing.T) {
	a := analyze.NewPython()

	res, err := a.Analyze(context.Background(), "testdata/py", allPyFiles)
	require.NoError(t, err)

	// decorated.py: decorated_func is @somedec-decorated; calls inner_target() which is
	// scoped (0.85 raw) but must be capped to 0.45 and carry degraded_by=decorator.
	edge, ok := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.decorated.decorated_func", "py:func:pkg.decorated.inner_target")
	require.True(t, ok, "expected decorated_func->inner_target calls edge")
	require.Equal(t, "0.45", edge.Properties["confidence"],
		"confidence must be capped at 0.45 for decorated callers")
	degradedBy := edge.Properties["degraded_by"]
	require.True(t, strings.Contains(degradedBy, "decorator"),
		"degraded_by must contain 'decorator', got %q", degradedBy)
}
