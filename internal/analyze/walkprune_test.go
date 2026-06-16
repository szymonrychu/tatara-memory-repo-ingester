package analyze_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// TestPythonRepoWalkSkipsDependencyDirs proves the repo-wide index that backs
// cross-file resolution does NOT index files under node_modules/.venv/etc.
// A unique symbol that exists only inside .venv must stay unknown, so a call to
// it dangles instead of resolving to a phantom global_name_match edge.
func TestPythonRepoWalkSkipsDependencyDirs(t *testing.T) {
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "pkg", "caller.py"),
		"def caller():\n    target()\n")
	// Same unique name defined only inside a dependency dir.
	mustWrite(t, filepath.Join(root, ".venv", "lib", "dep.py"),
		"def target():\n    pass\n")
	mustWrite(t, filepath.Join(root, "node_modules", "shadow.py"),
		"def target():\n    pass\n")

	a := analyze.NewPython()
	res, err := a.Analyze(context.Background(), root, []string{"pkg/caller.py"})
	require.NoError(t, err)

	for _, e := range res.Edges {
		require.NotContains(t, e.To, ".venv",
			"call resolved into .venv: dependency dir was indexed")
		require.NotContains(t, e.To, "node_modules",
			"call resolved into node_modules: dependency dir was indexed")
	}
	// With pruning, target() has no in-repo def -> no calls edge at all.
	_, hasEdge := findEdge(res.Edges, contract.RelCalls,
		"py:func:pkg.caller.caller", "py:func:.venv.lib.dep.target")
	require.False(t, hasEdge, "must not resolve to .venv target")
}

// TestJSRepoWalkSkipsDependencyDirs proves jsWalkRepo prunes node_modules so a
// name defined only there cannot pollute the repo-wide resolution index.
func TestJSRepoWalkSkipsDependencyDirs(t *testing.T) {
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "src", "app.js"),
		"function app() {\n  helper();\n}\n")
	mustWrite(t, filepath.Join(root, "node_modules", "dep", "index.js"),
		"function helper() {}\n")

	a := analyze.NewJavaScript()
	res, err := a.Analyze(context.Background(), root, []string{"src/app.js"})
	require.NoError(t, err)

	for _, e := range res.Edges {
		require.NotContains(t, e.To, "node_modules",
			"JS call resolved into node_modules: dependency dir was indexed")
		require.NotContains(t, e.From, "node_modules")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
