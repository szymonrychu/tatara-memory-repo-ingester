package analyze_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// helmTestdataDir returns the absolute path to the helm testdata directory.
func helmTestdataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "helm")
}

func TestHelmAnalyzer(t *testing.T) {
	repoRoot := helmTestdataDir()
	a := analyze.NewHelm(repoRoot)

	// Match assertions
	require.True(t, a.Match("mychart/templates/deployment.yaml"))
	require.True(t, a.Match("mychart/Chart.yaml"))
	require.True(t, a.Match("mychart/values.yaml"))
	require.False(t, a.Match("main.go"))
	require.False(t, a.Match("README.md"))

	files := []string{
		"mychart/Chart.yaml",
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
		"mychart/templates/conditions.yaml",
		"mychart/templates/broken.yaml",
	}
	res, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}

	// helm_chart entity
	require.True(t, ids["helm:chart:mychart"], "expected helm:chart:mychart entity")

	// helm_value entity for image.repository
	require.True(t, ids["helm:value:mychart.image.repository"], "expected helm:value:mychart.image.repository entity")

	// template entity
	tmplID := "helm:template:mychart/templates/deployment.yaml"
	require.True(t, ids[tmplID], "expected template entity")

	// value_ref edge: template -> helm:value:mychart.image.repository
	_, vref := findEdge(res.Edges, contract.RelValueRef, tmplID, "helm:value:mychart.image.repository")
	require.True(t, vref, "expected value_ref edge template->helm:value:mychart.image.repository")

	// includes edge: template -> helm:include:mychart.labels
	_, inc := findEdge(res.Edges, contract.RelIncludes, tmplID, "helm:include:mychart.labels")
	require.True(t, inc, "expected includes edge template->helm:include:mychart.labels")

	// subchart edge: helm:chart:mychart -> helm:chart:common (from dependencies)
	_, sub := findEdge(res.Edges, contract.RelSubchart, "helm:chart:mychart", "helm:chart:common")
	require.True(t, sub, "expected subchart edge mychart->common")

	// value_ref edges from control-node CONDITIONS (with/if/range .Pipe):
	// .Values.image appears ONLY in a `with` condition in conditions.yaml
	condTmplID := "helm:template:mychart/templates/conditions.yaml"
	_, withRef := findEdge(res.Edges, contract.RelValueRef, condTmplID, "helm:value:mychart.image")
	require.True(t, withRef, "expected value_ref edge from with .Values.image condition")

	// .Values.enabled appears ONLY in an `if` condition in conditions.yaml
	_, ifRef := findEdge(res.Edges, contract.RelValueRef, condTmplID, "helm:value:mychart.enabled")
	require.True(t, ifRef, "expected value_ref edge from if .Values.enabled condition")

	// resilience: broken.yaml is unparseable; analyzer must not crash and must still emit
	// entities/edges for deployment.yaml and conditions.yaml in the same chart
	require.True(t, ids[tmplID], "deployment template entity still present despite broken.yaml")
	require.True(t, ids[condTmplID], "conditions template entity still present despite broken.yaml")

	// CONTRACT: all FilePath/SrcFile/chunk paths must be repo-relative and within files set
	scope := map[string]bool{}
	for _, f := range files {
		scope[f] = true
	}

	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue
		}
		require.True(t, scope[e.FilePath],
			"entity %q has FilePath %q not in files set", e.ID, e.FilePath)
	}

	for _, e := range res.Edges {
		require.True(t, scope[e.SrcFile],
			"edge %q->%q has SrcFile %q not in files set", e.From, e.To, e.SrcFile)
	}

	for _, c := range res.Chunks {
		require.True(t, scope[c.FilePath],
			"chunk for entity %q has FilePath %q not in files set", c.EntityID, c.FilePath)
	}

	// At least one chunk emitted per template
	require.NotEmpty(t, res.Chunks)
}

// TestHelmAnalyzer_IncrementalWithoutChartYAML covers the confirmed live bug:
// incremental ingest where Chart.yaml was NOT modified (only templates/values changed).
// Chart.yaml exists on disk so the chart is parseable, but it is NOT in the diff files set.
// The helm_chart entity must use FilePath="" (repo-scoped; tatara-memory exempts
// empty file_path, commit 780b66f), and subchart edges must be OMITTED entirely
// (the server does NOT exempt an empty edge src_file).
func TestHelmAnalyzer_IncrementalWithoutChartYAML(t *testing.T) {
	repoRoot := helmTestdataDir()
	a := analyze.NewHelm(repoRoot)

	// Chart.yaml is intentionally absent from files (incremental: only templates changed)
	files := []string{
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
	}
	res, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)

	// helm_chart entity must be present (chart is still parsed from disk)
	var chartEntity *contract.Entity
	for i := range res.Entities {
		if res.Entities[i].ID == "helm:chart:mychart" {
			chartEntity = &res.Entities[i]
			break
		}
	}
	require.NotNil(t, chartEntity, "helm_chart entity must be emitted even when Chart.yaml not in files")

	// KEY assertion: FilePath must be empty (repo-scoped) because Chart.yaml is not in files
	require.Equal(t, "", chartEntity.FilePath,
		"helm_chart entity FilePath must be empty when Chart.yaml is not in the diff files set")

	// No subchart edge may be emitted: it is Chart.yaml-sourced and the server
	// rejects an empty edge src_file. mychart depends on common, so a buggy
	// analyzer would emit mychart->common here.
	for _, e := range res.Edges {
		require.NotEqual(t, contract.RelSubchart, e.Relation,
			"no subchart edge may be emitted when Chart.yaml is not in the diff (got %q->%q)", e.From, e.To)
	}

	// Scope contract: entity FilePath may be empty (repo-scoped) but every edge
	// src_file MUST be in the files set (the server does not exempt empty here).
	scope := map[string]bool{}
	for _, f := range files {
		scope[f] = true
	}
	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue
		}
		require.True(t, scope[e.FilePath],
			"entity %q has FilePath %q not in files set", e.ID, e.FilePath)
	}
	for _, e := range res.Edges {
		require.True(t, scope[e.SrcFile],
			"edge %q->%q has SrcFile %q not in files set", e.From, e.To, e.SrcFile)
	}
}

// TestHelmAnalyzer_FullIngestChartYAMLInFiles ensures the existing full-ingest behavior
// (Chart.yaml IS in files) is not regressed: FilePath must equal the Chart.yaml path.
func TestHelmAnalyzer_FullIngestChartYAMLInFiles(t *testing.T) {
	repoRoot := helmTestdataDir()
	a := analyze.NewHelm(repoRoot)

	files := []string{
		"mychart/Chart.yaml",
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
	}
	res, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)

	var chartEntity *contract.Entity
	for i := range res.Entities {
		if res.Entities[i].ID == "helm:chart:mychart" {
			chartEntity = &res.Entities[i]
			break
		}
	}
	require.NotNil(t, chartEntity, "helm_chart entity must be emitted")
	require.Equal(t, "mychart/Chart.yaml", chartEntity.FilePath,
		"helm_chart entity FilePath must equal Chart.yaml path when it is in files")

	// All subchart edges must have SrcFile = Chart.yaml path
	for _, e := range res.Edges {
		if e.Relation == contract.RelSubchart {
			require.Equal(t, "mychart/Chart.yaml", e.SrcFile,
				"subchart edge SrcFile must be Chart.yaml path when it is in files")
		}
	}
}

// TestHelmAnalyzer_RootChart verifies findings 1 and 3:
// a chart whose Chart.yaml/values.yaml/templates/ are all at the repo root.
// Without the fix, findChartRoot("templates/deployment.yaml") returned "templates"
// and chartYAMLPath was "templates/Chart.yaml" (not found), dropping all template entities.
// Also, chartYAMLPath was "./Chart.yaml" vs diff path "Chart.yaml" causing FilePath mismatch.
func TestHelmAnalyzer_RootChart(t *testing.T) {
	repoRoot := filepath.Join(helmTestdataDir(), "rootchart")
	a := analyze.NewHelm(repoRoot)

	// Match: templates/ at root should be matched (Chart.yaml exists on disk).
	require.True(t, a.Match("templates/deployment.yaml"),
		"templates/deployment.yaml must match when Chart.yaml is in the same dir")
	require.True(t, a.Match("Chart.yaml"))
	require.True(t, a.Match("values.yaml"))

	files := []string{
		"Chart.yaml",
		"values.yaml",
		"templates/deployment.yaml",
	}
	res, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range res.Entities {
		ids[e.ID] = true
	}

	// helm_chart entity
	require.True(t, ids["helm:chart:rootchart"], "expected helm:chart:rootchart entity; root-chart entities were dropped before fix")

	// helm_value entities
	require.True(t, ids["helm:value:rootchart.image.repository"],
		"expected helm:value:rootchart.image.repository; root-chart values were not emitted before fix")
	require.True(t, ids["helm:value:rootchart.replicaCount"],
		"expected helm:value:rootchart.replicaCount")

	// template entity
	tmplID := "helm:template:templates/deployment.yaml"
	require.True(t, ids[tmplID], "expected template entity for root chart; was dropped before fix")

	// helm_chart FilePath must be "Chart.yaml" (not "./Chart.yaml") - finding 1
	var chartEntity *contract.Entity
	for i := range res.Entities {
		if res.Entities[i].ID == "helm:chart:rootchart" {
			chartEntity = &res.Entities[i]
			break
		}
	}
	require.NotNil(t, chartEntity)
	require.Equal(t, "Chart.yaml", chartEntity.FilePath,
		"root-chart entity FilePath must be 'Chart.yaml', not './Chart.yaml'")

	// subchart edge for root chart
	_, sub := findEdge(res.Edges, contract.RelSubchart, "helm:chart:rootchart", "helm:chart:common")
	require.True(t, sub, "expected subchart edge rootchart->common")

	// value_ref edge from root-chart template
	_, vref := findEdge(res.Edges, contract.RelValueRef, tmplID, "helm:value:rootchart.image.repository")
	require.True(t, vref, "expected value_ref edge from root-chart template")

	// scope contract
	scope := map[string]bool{}
	for _, f := range files {
		scope[f] = true
	}
	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue
		}
		require.True(t, scope[e.FilePath], "entity %q FilePath %q not in files", e.ID, e.FilePath)
	}
	for _, e := range res.Edges {
		require.True(t, scope[e.SrcFile], "edge %q->%q SrcFile %q not in files", e.From, e.To, e.SrcFile)
	}
}

// TestHelmAnalyzer_MatchTightening verifies finding 2:
// templates/ files that are NOT inside a Helm chart must NOT be claimed by the helm analyzer.
func TestHelmAnalyzer_MatchTightening(t *testing.T) {
	repoRoot := filepath.Join(helmTestdataDir(), "nonchartrepo")
	a := analyze.NewHelm(repoRoot)

	// web/templates/index.html: web/ has no Chart.yaml, so helm should NOT claim it.
	require.False(t, a.Match("web/templates/index.html"),
		"helm Match must not claim a templates/ file when no Chart.yaml exists at the chart root")
}

// TestHelmAnalyzer_EmptyChartName verifies finding 8:
// a Chart.yaml with no name field must not emit an empty-named helm_chart entity.
func TestHelmAnalyzer_EmptyChartName(t *testing.T) {
	t.TempDir() // ensure temp cleanup
	td := t.TempDir()
	// Write a Chart.yaml with no name
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nversion: 0.1.0\n"))
	require.NoError(t, writeFile(td, "values.yaml", "key: val\n"))

	a := analyze.NewHelm(td)
	files := []string{"Chart.yaml", "values.yaml"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err)

	for _, e := range res.Entities {
		require.NotEqual(t, "helm:chart:", e.ID, "must not emit empty-named helm_chart entity")
		require.NotContains(t, e.ID, "helm:value:.", "must not emit malformed helm_value entities")
	}
	require.Empty(t, res.Entities, "no entities should be emitted when Chart.yaml has no name")

	// The emit-nothing half above is only half the contract. Both files are in the
	// diff set and both produced nothing, so both must be reported in FailedFiles
	// or the caller keeps them in the reconcile scope and purges their last-good
	// rows with no replacement.
	require.ElementsMatch(t, []string{"Chart.yaml", "values.yaml"}, res.FailedFiles,
		"every diff-set file of a nameless chart must be reported in FailedFiles")
}

// TestHelmAnalyzer_MissingChartNameRecordsWholeChart pins the blast radius of the
// missing-name branch: it skips the REST of the per-chart loop, so the chart's
// values.yaml and every one of its templates produce nothing too. All of them are
// diff-set files and all of them must be recorded.
func TestHelmAnalyzer_MissingChartNameRecordsWholeChart(t *testing.T) {
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nversion: 0.1.0\n"))
	require.NoError(t, writeFile(td, "values.yaml", "image:\n  repository: nginx\n"))
	require.NoError(t, mkdirFile(td, "templates/deployment.yaml", "image: {{ .Values.image.repository }}\n"))

	a := analyze.NewHelm(td)
	files := []string{"Chart.yaml", "values.yaml", "templates/deployment.yaml"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err, "a nameless Chart.yaml is a soft failure, not a batch error")

	require.Empty(t, res.Entities, "a nameless chart emits nothing")
	require.Empty(t, res.Chunks, "a nameless chart emits no chunks")
	require.ElementsMatch(t, files, res.FailedFiles,
		"the whole chart is skipped, so every diff-set file in it must be recorded")
}

// TestHelmAnalyzer_UnreadableChartYAMLRecordsWholeChart is the read-failure twin.
// Reachability is narrow but real: Match's os.Stat gate (helm.go) only proves
// Chart.yaml exists, not that it can be read, and the Chart.yaml-by-basename
// short-circuit claims a file whose resolved chart root has no Chart.yaml at all.
// A dangling symlink is used rather than chmod 000 so the test is uid-independent
// (chmod is a no-op as root).
func TestHelmAnalyzer_UnreadableChartYAMLRecordsWholeChart(t *testing.T) {
	td := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(td, "does-not-exist.yaml"), filepath.Join(td, "Chart.yaml")))
	require.NoError(t, writeFile(td, "values.yaml", "image:\n  repository: nginx\n"))
	require.NoError(t, mkdirFile(td, "templates/deployment.yaml", "image: {{ .Values.image.repository }}\n"))

	a := analyze.NewHelm(td)
	files := []string{"Chart.yaml", "values.yaml", "templates/deployment.yaml"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err, "an unreadable Chart.yaml is a soft failure, not a batch error")

	require.Empty(t, res.Entities, "an unreadable Chart.yaml emits nothing for the whole chart")
	require.ElementsMatch(t, files, res.FailedFiles,
		"the whole chart is skipped, so every diff-set file in it must be recorded")
}

// TestHelmAnalyzer_ContextChartYAMLNotRecorded guards the one way the fix is easy
// to get wrong: a Chart.yaml read purely for chart-name resolution is NOT a diff
// file, and putting it in FailedFiles poisons the caller's reconcile set with a
// path that was never in the push. Only cfiles (this chart's slice of the diff
// set) may be recorded.
func TestHelmAnalyzer_ContextChartYAMLNotRecorded(t *testing.T) {
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nversion: 0.1.0\n"))
	require.NoError(t, writeFile(td, "values.yaml", "key: val\n"))

	a := analyze.NewHelm(td)
	// Chart.yaml is deliberately NOT in the diff set; it is read for context only.
	res, err := a.Analyze(context.Background(), td, []string{"values.yaml"})
	require.NoError(t, err)

	require.Equal(t, []string{"values.yaml"}, res.FailedFiles,
		"only diff-set files may be recorded; a context-only Chart.yaml must not be")
}

// TestHelmAnalyzer_ChartRootFileWithNoRowsRecorded pins the third no-rows path in
// the per-chart loop, which is reached on a HEALTHY chart. Match with repoRoot set
// claims ANY path whose chart root has a Chart.yaml sibling, not just
// Chart.yaml/values.yaml/templates, and helm is registered before docs, so a
// chart's README.md is routed here. In Analyze it falls past chartYAMLPath, past
// the valuesPath loop and past the isTemplate filter with no else: zero rows, and
// until this test, zero record. Adding a Chart.yaml to a directory whose .md files
// docs had already ingested is enough to purge them.
//
// Who SHOULD own a chart's README is a routing question and is deliberately not
// answered here; see issue #43. This test pins only that the file is not silently
// dropped.
func TestHelmAnalyzer_ChartRootFileWithNoRowsRecorded(t *testing.T) {
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: healthy\nversion: 0.1.0\n"))
	require.NoError(t, writeFile(td, "values.yaml", "key: val\n"))
	require.NoError(t, writeFile(td, "README.md", "# healthy\n"))
	require.NoError(t, writeFile(td, "values-prod.yaml", "key: prod\n"))
	require.NoError(t, writeFile(td, ".helmignore", "*.tmp\n"))

	a := analyze.NewHelm(td)
	// All five are claimed by Match and all five are in the diff set.
	for _, f := range []string{"Chart.yaml", "values.yaml", "README.md", "values-prod.yaml", ".helmignore"} {
		require.True(t, a.Match(f), "Match claims %q, which is why Analyze must account for it", f)
	}
	files := []string{"Chart.yaml", "values.yaml", "README.md", "values-prod.yaml", ".helmignore"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"README.md", "values-prod.yaml", ".helmignore"}, res.FailedFiles,
		"a claimed diff-set file the chart loop emits nothing for must be recorded")
	require.NotContains(t, res.FailedFiles, "Chart.yaml", "Chart.yaml produced the chart entity")
	require.NotContains(t, res.FailedFiles, "values.yaml", "values.yaml produced helm:value entities")
}

// TestHelmAnalyzer_UnreadableTemplateRecordedAsFailed pins the read-failure half
// of the template contract: an unreadable template emits its helm_template entity
// but no edges and no chunk, so it must be reported in FailedFiles - exactly as
// the parse-failure branch does. Without it the file stays in both reconcile
// scopes and its last-good edges are purged with no replacement.
func TestHelmAnalyzer_UnreadableTemplateRecordedAsFailed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root: chmod 000 is ineffective as root")
	}
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: unreadable\nversion: 0.1.0\n"))
	require.NoError(t, mkdirFile(td, "templates/deployment.yaml", "kind: Deployment\n"))
	tmpl := filepath.Join(td, "templates", "deployment.yaml")
	require.NoError(t, os.Chmod(tmpl, 0o000))
	t.Cleanup(func() { _ = os.Chmod(tmpl, 0o600) })

	a := analyze.NewHelm(td)
	files := []string{"Chart.yaml", "templates/deployment.yaml"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err, "an unreadable template is a per-file soft failure, not a batch error")
	require.Contains(t, res.FailedFiles, "templates/deployment.yaml",
		"unreadable template must be reported in FailedFiles")
}

// TestHelmAnalyzer_EdgeDedup verifies finding 5:
// duplicate value_ref and includes edges from the same template are emitted only once.
func TestHelmAnalyzer_EdgeDedup(t *testing.T) {
	td := t.TempDir()
	// Chart.yaml
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: deduptest\nversion: 0.1.0\n"))
	// values.yaml
	require.NoError(t, writeFile(td, "values.yaml", "image:\n  repository: nginx\n"))
	// Template that references .Values.image.repository twice and includes the same helper twice
	require.NoError(t, mkdirFile(td, "templates/dup.yaml",
		`image1: {{ .Values.image.repository }}
image2: {{ .Values.image.repository }}
{{ include "deduptest.labels" . }}
{{ include "deduptest.labels" . }}
`))

	a := analyze.NewHelm(td)
	files := []string{"Chart.yaml", "values.yaml", "templates/dup.yaml"}
	res, err := a.Analyze(context.Background(), td, files)
	require.NoError(t, err)

	tmplID := "helm:template:templates/dup.yaml"
	// Count value_ref edges to helm:value:deduptest.image.repository
	vrCount := 0
	incCount := 0
	for _, e := range res.Edges {
		if e.From == tmplID && e.Relation == contract.RelValueRef && e.To == "helm:value:deduptest.image.repository" {
			vrCount++
		}
		if e.From == tmplID && e.Relation == contract.RelIncludes && e.To == "helm:include:deduptest.labels" {
			incCount++
		}
	}
	require.Equal(t, 1, vrCount, "duplicate value_ref edges must be deduplicated to 1")
	require.Equal(t, 1, incCount, "duplicate includes edges must be deduplicated to 1")
}

// TestHelmAnalyzer_DeterministicOutput verifies finding 10:
// repeated Analyze calls must produce identical entity/edge ordering.
func TestHelmAnalyzer_DeterministicOutput(t *testing.T) {
	repoRoot := helmTestdataDir()
	a := analyze.NewHelm(repoRoot)

	files := []string{
		"mychart/Chart.yaml",
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
		"mychart/templates/conditions.yaml",
	}

	res1, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)
	res2, err := a.Analyze(context.Background(), repoRoot, files)
	require.NoError(t, err)

	require.Equal(t, len(res1.Entities), len(res2.Entities), "entity count must be deterministic")
	for i := range res1.Entities {
		require.Equal(t, res1.Entities[i].ID, res2.Entities[i].ID,
			"entity[%d] ID must be deterministic across runs", i)
	}
}

// TestHelmAnalyzer_MatchValuesYAMLRequiresChartYAMLSibling verifies finding 1 (round 2):
// Match("config/values.yaml") must return false when no Chart.yaml sibling exists on disk,
// and true when a Chart.yaml sibling is present. Each case uses a fresh analyzer instance
// (no shared cache state) to test independently.
func TestHelmAnalyzer_MatchValuesYAMLRequiresChartYAMLSibling(t *testing.T) {
	t.Run("no_sibling_returns_false", func(t *testing.T) {
		td := t.TempDir()
		// config/values.yaml with NO Chart.yaml sibling.
		require.NoError(t, mkdirFile(td, "config/values.yaml", "key: val\n"))
		a := analyze.NewHelm(td)
		require.False(t, a.Match("config/values.yaml"),
			"Match must not claim config/values.yaml when no Chart.yaml sibling exists")
	})

	t.Run("with_sibling_returns_true", func(t *testing.T) {
		td := t.TempDir()
		// config/values.yaml WITH a Chart.yaml sibling.
		require.NoError(t, mkdirFile(td, "config/values.yaml", "key: val\n"))
		require.NoError(t, mkdirFile(td, "config/Chart.yaml", "apiVersion: v2\nname: cfg\nversion: 0.1.0\n"))
		a := analyze.NewHelm(td)
		require.True(t, a.Match("config/values.yaml"),
			"Match must claim config/values.yaml when Chart.yaml sibling exists on disk")
	})
}

// TestHelmAnalyzer_MatchValuesYAMLRootRequiresChartYAML verifies that a root-level values.yaml
// also requires a Chart.yaml in the same dir when repoRoot is set.
func TestHelmAnalyzer_MatchValuesYAMLRootRequiresChartYAML(t *testing.T) {
	t.Run("no_chart_yaml_returns_false", func(t *testing.T) {
		td := t.TempDir()
		require.NoError(t, writeFile(td, "values.yaml", "key: val\n"))
		a := analyze.NewHelm(td)
		require.False(t, a.Match("values.yaml"),
			"Match must not claim root values.yaml when no Chart.yaml sibling exists")
	})

	t.Run("with_chart_yaml_returns_true", func(t *testing.T) {
		td := t.TempDir()
		require.NoError(t, writeFile(td, "values.yaml", "key: val\n"))
		require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: rooty\nversion: 0.1.0\n"))
		a := analyze.NewHelm(td)
		require.True(t, a.Match("values.yaml"),
			"Match must claim root values.yaml when Chart.yaml sibling exists")
	})
}

// TestHelmAnalyzer_MatchStatMemoization verifies finding 2 (round 2):
// repeated Match calls for templates/ files in the same chart root must not re-stat
// the same Chart.yaml. We test correctness of the memoization path (cache hit returns
// same result). Stat-count verification is implicit: if the implementation is correct,
// removing the chart root's Chart.yaml AFTER the first match call must not change the
// result for a second call (the cached true is returned without re-stat).
func TestHelmAnalyzer_MatchStatMemoization(t *testing.T) {
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: cachechart\nversion: 0.1.0\n"))
	require.NoError(t, mkdirFile(td, "templates/a.yaml", "a: 1\n"))
	require.NoError(t, mkdirFile(td, "templates/b.yaml", "b: 2\n"))

	a := analyze.NewHelm(td)

	// Both files match (Chart.yaml present).
	require.True(t, a.Match("templates/a.yaml"))
	require.True(t, a.Match("templates/b.yaml"))

	// Remove Chart.yaml to prove the second call used the cache.
	require.NoError(t, os.Remove(filepath.Join(td, "Chart.yaml")))

	// The memoized result must still be true (cache hit, no re-stat).
	require.True(t, a.Match("templates/b.yaml"),
		"Match must return cached result (true) for same chart root after Chart.yaml removed")
}

// writeFile writes content to path relative to dir.
func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
}

// mkdirFile creates parent dirs and writes content.
func mkdirFile(dir, name, content string) error {
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o600)
}

// TestHelmAnalyzer_UnreadableValuesRecordedAsFailed is the values.yaml half of the
// same read-vs-parse asymmetry: the parse branch records FailedFiles, the read
// branch did not, so an unreadable values.yaml in the diff purged every
// helm:value entity for its chart with no replacement.
func TestHelmAnalyzer_UnreadableValuesRecordedAsFailed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root: chmod 000 is ineffective as root")
	}
	td := t.TempDir()
	require.NoError(t, writeFile(td, "Chart.yaml", "apiVersion: v2\nname: unreadablevalues\nversion: 0.1.0\n"))
	require.NoError(t, writeFile(td, "values.yaml", "replicaCount: 1\n"))
	values := filepath.Join(td, "values.yaml")
	require.NoError(t, os.Chmod(values, 0o000))
	t.Cleanup(func() { _ = os.Chmod(values, 0o600) })

	a := analyze.NewHelm(td)
	res, err := a.Analyze(context.Background(), td, []string{"Chart.yaml", "values.yaml"})
	require.NoError(t, err, "an unreadable values.yaml is a per-file soft failure, not a batch error")
	require.Contains(t, res.FailedFiles, "values.yaml",
		"unreadable values.yaml must be reported in FailedFiles")
}
