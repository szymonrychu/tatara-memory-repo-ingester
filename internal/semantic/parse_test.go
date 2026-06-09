package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

const sampleFragment = `{
  "nodes": [
    {"id":"auth_session_validatetoken","label":"Validate Token","file_type":"code","source_file":"auth/session.go"},
    {"id":"retry_backoff","label":"Retry Backoff","file_type":"concept","source_file":"http/client.go"},
    {"id":"why_jitter","label":"Why Jitter","file_type":"rationale","source_file":"http/client.go"}
  ],
  "edges": [
    {"source":"auth_session_validatetoken","target":"retry_backoff","relation":"semantically_similar_to","confidence":"INFERRED","confidence_score":0.85,"source_file":"http/client.go","weight":1.0},
    {"source":"why_jitter","target":"retry_backoff","relation":"rationale_for","confidence":"EXTRACTED","confidence_score":1.0,"source_file":"http/client.go","weight":1.0},
    {"source":"a","target":"b","relation":"calls","confidence":"AMBIGUOUS","confidence_score":0.2,"source_file":"http/client.go"}
  ],
  "hyperedges": [
    {"id":"auth_flow","label":"Auth Flow","nodes":["auth_session_validatetoken","retry_backoff","why_jitter"],"relation":"participate_in","confidence":"INFERRED","confidence_score":0.75,"source_file":"auth/session.go"}
  ],
  "input_tokens": 0,
  "output_tokens": 0
}`

func TestParseFragmentConceptNodeIDs(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)
	// code file_type nodes are NOT emitted as entities (they reference AST nodes).
	// concept/rationale file_type nodes ARE emitted with deterministic ids.
	byType := map[string]contract.Entity{}
	for _, e := range res.Entities {
		byType[e.Type] = e
	}
	require.Contains(t, byType, contract.EntityConcept)
	require.Contains(t, byType, contract.EntityRationale)
	require.Equal(t, "concept:myrepo:retry-backoff", byType[contract.EntityConcept].ID)
	require.Equal(t, "concept:myrepo:why-jitter", byType[contract.EntityRationale].ID)
	for _, e := range res.Entities {
		require.Equal(t, "http/client.go", e.FilePath)
	}
}

func TestParseFragmentEdgesMapToContract(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)
	require.Len(t, res.Edges, 3)
	var sim contract.Edge
	for _, e := range res.Edges {
		if e.Relation == contract.RelSemanticallySimilar {
			sim = e
		}
	}
	require.Equal(t, contract.RelSemanticallySimilar, sim.Relation)
	require.InDelta(t, 0.85, sim.ConfidenceScore, 1e-9)
	require.Equal(t, contract.TierInferred, sim.ConfidenceTier)
	require.Equal(t, "http/client.go", sim.SrcFile)
}

func TestParseFragmentConfidenceTiers(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)
	tiers := map[string]string{}
	for _, e := range res.Edges {
		tiers[e.Relation] = e.ConfidenceTier
	}
	require.Equal(t, contract.TierInferred, tiers[contract.RelSemanticallySimilar]) // 0.85
	require.Equal(t, contract.TierExtracted, tiers[contract.RelRationaleFor])       // 1.0
	require.Equal(t, contract.TierAmbiguous, tiers[contract.RelCalls])              // 0.2
}

func TestParseFragmentHyperedges(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 1)
	h := res.Hyperedges[0]
	require.Equal(t, "Auth Flow", h.Label)
	require.Equal(t, "participate_in", h.Relation)
	require.InDelta(t, 0.75, h.ConfidenceScore, 1e-9)
	require.Equal(t, "auth/session.go", h.SrcFile)
	require.Len(t, h.Members, 3)
	require.Equal(t, "he:myrepo:auth/session.go:auth_flow", h.ID)
}

func TestParseFragmentHyperedgesCappedAtThree(t *testing.T) {
	frag := `{"nodes":[],"edges":[],"hyperedges":[
	  {"id":"h1","label":"a","nodes":["x","y","z"],"relation":"form","confidence_score":0.7,"source_file":"f.go"},
	  {"id":"h2","label":"b","nodes":["x","y","z"],"relation":"form","confidence_score":0.7,"source_file":"f.go"},
	  {"id":"h3","label":"c","nodes":["x","y","z"],"relation":"form","confidence_score":0.7,"source_file":"f.go"},
	  {"id":"h4","label":"d","nodes":["x","y","z"],"relation":"form","confidence_score":0.7,"source_file":"f.go"}
	]}`
	res, err := ParseFragment("r", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 3)
}

func TestParseFragmentRejectsMalformedJSON(t *testing.T) {
	_, err := ParseFragment("r", []byte("not json"))
	require.Error(t, err)
}

func TestParseFragmentStripsCodeFences(t *testing.T) {
	wrapped := "```json\n" + sampleFragment + "\n```"
	res, err := ParseFragment("myrepo", []byte(wrapped))
	require.NoError(t, err)
	require.Len(t, res.Edges, 3)
}

func TestSlugLabel(t *testing.T) {
	require.Equal(t, "retry-backoff", slugLabel("Retry Backoff"))
	require.Equal(t, "why-jitter", slugLabel("Why Jitter!"))
	require.Equal(t, "a-b-c", slugLabel("a  b   c"))
	require.Equal(t, "leadingtrailing", slugLabel("  LeadingTrailing  "))
}
