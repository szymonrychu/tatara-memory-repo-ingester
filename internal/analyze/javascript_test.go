package analyze_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// allJSFiles is the full fixture set used by multi-file tests.
var allJSFiles = []string{
	"src/app.js",
	"src/util.js",
	"src/global_only.js",
	"src/uses_global.js",
	"src/dup_a.js",
	"src/dup_b.js",
	"src/uses_ambiguous.js",
	"src/dynamic.js",
}

// TestJavaScriptAnalyzer_Base pins the plan's base assertions:
// - f->g scoped call in app.js
// - app.js imports util.js
func TestJavaScriptAnalyzer_Base(t *testing.T) {
	a := analyze.NewJavaScript()
	require.True(t, a.Match("src/app.js"))
	require.True(t, a.Match("src/app.mjs"))
	require.True(t, a.Match("src/app.cjs"))
	require.False(t, a.Match("README.md"))

	res, err := a.Analyze(context.Background(), "testdata/js", []string{"src/app.js", "src/util.js"})
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}
	require.True(t, ids["js:module:src/app.js"])
	require.True(t, ids["js:func:src/app.js::f"])
	require.True(t, ids["js:func:src/app.js::g"])
	require.True(t, ids["js:module:src/util.js"])
	require.True(t, ids["js:func:src/util.js::h"])

	// defines edges
	_, defG := findEdge(res.Edges, contract.RelDefines, "js:module:src/app.js", "js:func:src/app.js::g")
	require.True(t, defG, "expected module->g defines edge")
	_, defF := findEdge(res.Edges, contract.RelDefines, "js:module:src/app.js", "js:func:src/app.js::f")
	require.True(t, defF, "expected module->f defines edge")

	// scoped call: f->g
	call, ok := findEdge(res.Edges, contract.RelCalls, "js:func:src/app.js::f", "js:func:src/app.js::g")
	require.True(t, ok, "expected f->g calls edge")
	require.Equal(t, contract.ResScopedNameMatch, call.Properties["resolution"])
	require.Equal(t, "0.85", call.Properties["confidence"])

	// ES import edge: app.js -> util.js
	_, imp := findEdge(res.Edges, contract.RelImports, "js:module:src/app.js", "js:module:src/util.js")
	require.True(t, imp, "expected app.js imports util.js edge")
}

// TestJavaScriptAnalyzer_ImportedNameMatch: f in app.js calls h from util.js (imported tier).
func TestJavaScriptAnalyzer_ImportedNameMatch(t *testing.T) {
	a := analyze.NewJavaScript()

	res, err := a.Analyze(context.Background(), "testdata/js", []string{"src/app.js", "src/util.js"})
	require.NoError(t, err)

	edge, ok := findEdge(res.Edges, contract.RelCalls, "js:func:src/app.js::f", "js:func:src/util.js::h")
	require.True(t, ok, "expected f->h imported_name_match edge")
	require.Equal(t, contract.ResImportedNameMatch, edge.Properties["resolution"])
	require.Equal(t, "0.7", edge.Properties["confidence"])
}

// TestJavaScriptAnalyzer_GlobalNameMatch: globalCaller calls uniqueGlobal without importing it.
func TestJavaScriptAnalyzer_GlobalNameMatch(t *testing.T) {
	a := analyze.NewJavaScript()

	res, err := a.Analyze(context.Background(), "testdata/js", allJSFiles)
	require.NoError(t, err)

	edge, ok := findEdge(res.Edges, contract.RelCalls,
		"js:func:src/uses_global.js::globalCaller",
		"js:func:src/global_only.js::uniqueGlobal")
	require.True(t, ok, "expected globalCaller->uniqueGlobal global_name_match edge")
	require.Equal(t, contract.ResGlobalNameMatch, edge.Properties["resolution"])
	require.Equal(t, "0.45", edge.Properties["confidence"])
}

// TestJavaScriptAnalyzer_AmbiguousMultiDef: ambiguousCaller calls sharedName defined in both dup_a and dup_b.
func TestJavaScriptAnalyzer_AmbiguousMultiDef(t *testing.T) {
	a := analyze.NewJavaScript()

	res, err := a.Analyze(context.Background(), "testdata/js", allJSFiles)
	require.NoError(t, err)

	edgeA, okA := findEdge(res.Edges, contract.RelCalls,
		"js:func:src/uses_ambiguous.js::ambiguousCaller",
		"js:func:src/dup_a.js::sharedName")
	edgeB, okB := findEdge(res.Edges, contract.RelCalls,
		"js:func:src/uses_ambiguous.js::ambiguousCaller",
		"js:func:src/dup_b.js::sharedName")
	require.True(t, okA, "expected ambiguous edge to dup_a.sharedName")
	require.True(t, okB, "expected ambiguous edge to dup_b.sharedName")
	require.Equal(t, contract.ResAmbiguousMultiDef, edgeA.Properties["resolution"])
	require.Equal(t, "0.2", edgeA.Properties["confidence"])
	require.Equal(t, contract.ResAmbiguousMultiDef, edgeB.Properties["resolution"])
	require.Equal(t, "0.2", edgeB.Properties["confidence"])
}

// TestJavaScriptAnalyzer_DegradedDynamic: a computed member-expression call triggers degraded_by.
func TestJavaScriptAnalyzer_DegradedDynamic(t *testing.T) {
	a := analyze.NewJavaScript()

	res, err := a.Analyze(context.Background(), "testdata/js", allJSFiles)
	require.NoError(t, err)

	var dynamicCallerEntity *contract.Entity
	for i, e := range res.Entities {
		if e.ID == "js:func:src/dynamic.js::dynamicCaller" {
			dynamicCallerEntity = &res.Entities[i]
			break
		}
	}
	require.NotNil(t, dynamicCallerEntity, "expected dynamicCaller entity")
	dangling := dynamicCallerEntity.Properties["dangling_call"]
	degradedBy := dynamicCallerEntity.Properties["degraded_by"]
	// Pin both paths explicitly - dropping either recording must cause a failure.
	require.NotEmpty(t, dangling, "expected dangling_call set for obj[method]() call")
	require.Contains(t, degradedBy, "dynamic", "expected degraded_by to contain 'dynamic'")
}

// TestJavaScriptAnalyzer_RequireImport: CommonJS require() without extension resolves to .js module.
func TestJavaScriptAnalyzer_RequireImport(t *testing.T) {
	a := analyze.NewJavaScript()

	files := []string{"src/app.js", "src/util.js", "src/cjs_consumer.js"}
	res, err := a.Analyze(context.Background(), "testdata/js", files)
	require.NoError(t, err)

	// cjs_consumer.js does: const x = require('./util')  (no extension)
	// The import edge must resolve to js:module:src/util.js
	_, imp := findEdge(res.Edges, contract.RelImports, "js:module:src/cjs_consumer.js", "js:module:src/util.js")
	require.True(t, imp, "expected cjs_consumer.js imports src/util.js via require() with .js appended")
}

// TestJavaScriptAnalyzer_Unresolved: a call to a plain undefined identifier produces no calls edge
// and leaves a dangling_call property on the caller.
func TestJavaScriptAnalyzer_Unresolved(t *testing.T) {
	a := analyze.NewJavaScript()

	files := []string{"src/unresolved_caller.js"}
	res, err := a.Analyze(context.Background(), "testdata/js", files)
	require.NoError(t, err)

	// No calls edge to any 'nowhere' target must exist.
	_, ok := findEdge(res.Edges, contract.RelCalls, "js:func:src/unresolved_caller.js::u", "js:func:src/unresolved_caller.js::nowhere")
	require.False(t, ok, "expected no calls edge for undefined callee 'nowhere'")
	// A second scan: no edge to nowhere from any source in the result.
	for _, e := range res.Edges {
		if e.Relation == contract.RelCalls && strings.HasSuffix(e.To, "::nowhere") {
			t.Fatalf("found unexpected calls edge to nowhere: %+v", e)
		}
	}

	// The caller entity must record dangling_call.
	var callerEntity *contract.Entity
	for i, e := range res.Entities {
		if e.ID == "js:func:src/unresolved_caller.js::u" {
			callerEntity = &res.Entities[i]
			break
		}
	}
	require.NotNil(t, callerEntity, "expected entity js:func:src/unresolved_caller.js::u")
	require.NotEmpty(t, callerEntity.Properties["dangling_call"], "expected dangling_call for call to undefined 'nowhere'")
}
