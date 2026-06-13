package analyze_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestHelmAnalyzer(t *testing.T) {
	a := analyze.NewHelm()

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
	res, err := a.Analyze(context.Background(), "testdata/helm", files)
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
	a := analyze.NewHelm()

	// Chart.yaml is intentionally absent from files (incremental: only templates changed)
	files := []string{
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
	}
	res, err := a.Analyze(context.Background(), "testdata/helm", files)
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
	a := analyze.NewHelm()

	files := []string{
		"mychart/Chart.yaml",
		"mychart/values.yaml",
		"mychart/templates/deployment.yaml",
	}
	res, err := a.Analyze(context.Background(), "testdata/helm", files)
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
