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
		if e.SrcFile == "" {
			continue
		}
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
