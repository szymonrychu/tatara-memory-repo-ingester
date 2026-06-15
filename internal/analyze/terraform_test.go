package analyze_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

func TestTerraformAnalyzer(t *testing.T) {
	a := analyze.NewTerraform()
	require.True(t, a.Match("main.tf"))
	require.False(t, a.Match("main.go"))
	require.False(t, a.Match("values.yaml"))

	res, err := a.Analyze(context.Background(), "testdata/tf", []string{"main.tf"})
	require.NoError(t, err)

	ids := map[string]string{}
	for _, e := range res.Entities {
		ids[e.ID] = e.Type
	}

	// Entities
	require.Equal(t, contract.EntityTFVariable, ids["tf:variable:name"], "tf_variable entity")
	require.Equal(t, contract.EntityTFResource, ids["tf:resource:null_resource.a"], "tf_resource entity a")
	require.Equal(t, contract.EntityTFResource, ids["tf:resource:null_resource.b"], "tf_resource entity b")
	require.Equal(t, contract.EntityTFModule, ids["tf:module:child"], "tf_module entity")
	require.Equal(t, contract.EntityTFOutput, ids["tf:output:id"], "tf_output entity")
	require.Equal(t, contract.EntityTFData, ids["tf:data:aws_ami.ubuntu"], "tf_data entity")

	// var_ref: resource null_resource.a -> variable name
	varRefEdge, ok := findEdge(res.Edges, contract.RelVarRef, "tf:resource:null_resource.a", "tf:variable:name")
	require.True(t, ok, "resource should have var_ref to tf:variable:name")
	require.Equal(t, contract.ResTypeResolved, varRefEdge.Properties["resolution"])
	require.Equal(t, contract.ConfidenceFor(contract.ResTypeResolved), varRefEdge.Properties["confidence"])

	// references: output id -> resource null_resource.a
	_, ok = findEdge(res.Edges, contract.RelReferences, "tf:output:id", "tf:resource:null_resource.a")
	require.True(t, ok, "output should have references edge to tf:resource:null_resource.a")

	// depends_on: null_resource.a -> null_resource.b
	_, ok = findEdge(res.Edges, contract.RelDependsOn, "tf:resource:null_resource.a", "tf:resource:null_resource.b")
	require.True(t, ok, "null_resource.a should have depends_on edge to null_resource.b")

	// module_source: module child -> source path
	_, ok = findEdge(res.Edges, contract.RelModuleSource, "tf:module:child", "./modules/child")
	require.True(t, ok, "module child should have module_source edge")

	// data reference: null_resource.c -> data aws_ami.ubuntu (finding 1 + 2)
	_, ok = findEdge(res.Edges, contract.RelReferences, "tf:resource:null_resource.c", "tf:data:aws_ami.ubuntu")
	require.True(t, ok, "null_resource.c should reference tf:data:aws_ami.ubuntu (not tf:resource:data.aws_ami)")

	// depends_on data: null_resource.c -> data aws_ami.ubuntu (finding 1 + 2)
	_, ok = findEdge(res.Edges, contract.RelDependsOn, "tf:resource:null_resource.c", "tf:data:aws_ami.ubuntu")
	require.True(t, ok, "null_resource.c should have depends_on tf:data:aws_ami.ubuntu")

	// builtin roots must NOT produce any tf:resource:local.*, tf:resource:each.*, etc. edges (finding 2)
	builtinPrefixes := []string{
		"tf:resource:local.",
		"tf:resource:each.",
		"tf:resource:count.",
		"tf:resource:path.",
		"tf:resource:self.",
		"tf:resource:terraform.",
		"tf:resource:data.",
	}
	for _, e := range res.Edges {
		for _, prefix := range builtinPrefixes {
			require.False(t, strings.HasPrefix(e.To, prefix),
				"spurious edge to builtin/data pseudo-resource %q (from %q)", e.To, e.From)
		}
	}

	// Contract: all entity FilePaths are in the files set; all edge SrcFiles are in the files set
	filesScope := map[string]bool{"main.tf": true}
	for _, e := range res.Entities {
		if e.FilePath == "" {
			continue
		}
		require.True(t, filesScope[e.FilePath], "entity %q has FilePath %q not in files set", e.ID, e.FilePath)
	}
	for _, e := range res.Edges {
		if e.SrcFile == "" {
			continue
		}
		require.True(t, filesScope[e.SrcFile], "edge %q->%q has SrcFile %q not in files set", e.From, e.To, e.SrcFile)
	}
	for _, c := range res.Chunks {
		if c.FilePath == "" {
			continue
		}
		require.True(t, filesScope[c.FilePath], "chunk for entity %q has FilePath %q not in files set", c.EntityID, c.FilePath)
	}
}
