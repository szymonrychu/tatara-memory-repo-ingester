package semantic

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// sampleFragment uses model-emitted concept node ids ("retry_backoff", "why_jitter")
// in edge endpoints - exactly as the LLM would produce them. ParseFragment must remap
// those to their canonical concept:<repo>:<slug> form so edges are not dangling.
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

// TestParseFragmentConceptEdgesRemapped asserts that edge endpoints whose model id
// matches a concept/rationale node in the fragment are rewritten to their canonical
// concept:<repo>:<slug> form. Without this remap the edges are dangling - the entity
// is emitted as "concept:myrepo:retry-backoff" but the edge target stays "retry_backoff".
func TestParseFragmentConceptEdgesRemapped(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)

	edgesByRelation := map[string]contract.Edge{}
	for _, e := range res.Edges {
		edgesByRelation[e.Relation] = e
	}

	// semantically_similar_to: target "retry_backoff" -> "concept:myrepo:retry-backoff"
	sim := edgesByRelation[contract.RelSemanticallySimilar]
	require.Equal(t, "auth_session_validatetoken", sim.From,
		"non-concept source must pass through unchanged")
	require.Equal(t, "concept:myrepo:retry-backoff", sim.To,
		"concept node model id must be remapped to canonical concept:<repo>:<slug>")

	// rationale_for: source "why_jitter" -> "concept:myrepo:why-jitter", target "retry_backoff" -> "concept:myrepo:retry-backoff"
	rat := edgesByRelation[contract.RelRationaleFor]
	require.Equal(t, "concept:myrepo:why-jitter", rat.From,
		"rationale node model id must be remapped to canonical concept:<repo>:<slug>")
	require.Equal(t, "concept:myrepo:retry-backoff", rat.To,
		"concept node model id must be remapped to canonical concept:<repo>:<slug>")

	// calls: both endpoints are non-concept; must pass through unchanged
	calls := edgesByRelation[contract.RelCalls]
	require.Equal(t, "a", calls.From)
	require.Equal(t, "b", calls.To)
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
	// IDs are now slugified to avoid ':' delimiter collisions.
	require.Equal(t, "he:myrepo:auth-session-go:auth-flow", h.ID)
}

// TestParseFragmentHyperedgeMembersRemapped asserts that hyperedge members that
// match a concept/rationale node id are rewritten to their canonical ids, exactly
// as edge endpoints are via remapID (finding 2).
func TestParseFragmentHyperedgeMembersRemapped(t *testing.T) {
	res, err := ParseFragment("myrepo", []byte(sampleFragment))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 1)
	members := res.Hyperedges[0].Members
	// auth_session_validatetoken: non-concept, passes through unchanged.
	require.Equal(t, "auth_session_validatetoken", members[0])
	// retry_backoff -> concept:myrepo:retry-backoff
	require.Equal(t, "concept:myrepo:retry-backoff", members[1])
	// why_jitter -> concept:myrepo:why-jitter
	require.Equal(t, "concept:myrepo:why-jitter", members[2])
}

// TestParseFragmentHyperedgeIDSlugified asserts ':' in source_file does not break
// the id delimiter scheme, and that raw model ids with non-slug chars are normalized
// (finding 1).
func TestParseFragmentHyperedgeIDSlugified(t *testing.T) {
	frag := `{"nodes":[],"edges":[],"hyperedges":[
	  {"id":"my:edge","label":"x","nodes":["a","b","c"],"relation":"form","confidence_score":0.7,"source_file":"path/to:file.go"}
	]}`
	res, err := ParseFragment("r", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 1)
	// ':' in source_file and id must be slugified to '-'.
	require.Equal(t, "he:r:path-to-file-go:my-edge", res.Hyperedges[0].ID)
}

// TestParseFragmentHyperedgeSkipsFewerThanThreeMembers asserts that hyperedges with
// fewer than 3 members (contract says 3+) are dropped (finding 1).
func TestParseFragmentHyperedgeSkipsFewerThanThreeMembers(t *testing.T) {
	frag := `{"nodes":[],"edges":[],"hyperedges":[
	  {"id":"too-small","label":"x","nodes":["a","b"],"relation":"form","confidence_score":0.7,"source_file":"f.go"},
	  {"id":"just-right","label":"y","nodes":["a","b","c"],"relation":"form","confidence_score":0.7,"source_file":"f.go"}
	]}`
	res, err := ParseFragment("r", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 1)
	require.Equal(t, "he:r:f-go:just-right", res.Hyperedges[0].ID)
}

// TestParseFragmentHyperedgeFallbackIDOnEmptyFields asserts that when source_file
// or id are empty the id falls back to a hash of members+label (finding 1).
func TestParseFragmentHyperedgeFallbackIDOnEmptyFields(t *testing.T) {
	frag := `{"nodes":[],"edges":[],"hyperedges":[
	  {"id":"","label":"Auth Flow","nodes":["a","b","c"],"relation":"form","confidence_score":0.7,"source_file":""}
	]}`
	res, err := ParseFragment("r", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Hyperedges, 1)
	// Must be a stable fallback id, not "he:r::" which collides.
	require.NotEqual(t, "he:r::", res.Hyperedges[0].ID)
	require.True(t, len(res.Hyperedges[0].ID) > len("he:r::"))
}

// TestParseFragmentEdgesSkipEmptyOrSelfEndpoints asserts that edges with empty
// From/To or self-loops are dropped (finding 4).
func TestParseFragmentEdgesSkipEmptyOrSelfEndpoints(t *testing.T) {
	frag := `{"nodes":[],"edges":[
	  {"source":"","target":"x","relation":"calls","confidence_score":0.8,"source_file":"f.go"},
	  {"source":"a","target":"","relation":"calls","confidence_score":0.8,"source_file":"f.go"},
	  {"source":"a","target":"a","relation":"calls","confidence_score":0.8,"source_file":"f.go"},
	  {"source":"a","target":"b","relation":"calls","confidence_score":0.8,"source_file":"f.go"}
	],"hyperedges":[]}`
	res, err := ParseFragment("r", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Edges, 1)
	require.Equal(t, "a", res.Edges[0].From)
	require.Equal(t, "b", res.Edges[0].To)
}

// TestStripFencesRobust asserts that stripFences extracts valid JSON even with
// unusual fence variations: uppercase JSON, space after backticks, trailing prose,
// or a leading fence with no newline before the opening brace (finding 3).
func TestStripFencesRobust(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"bare json", `{"nodes":[],"edges":[],"hyperedges":[]}`},
		{"backtick-json fence", "```json\n{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\n```"},
		{"bare backtick fence", "```\n{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\n```"},
		{"uppercase JSON fence", "```JSON\n{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\n```"},
		{"space after backtick", "``` json\n{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\n```"},
		{"trailing prose", "{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\nSome explanation here."},
		{"fence with trailing prose", "```json\n{\"nodes\":[],\"edges\":[],\"hyperedges\":[]}\n```\nExtra text."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseFragment("r", []byte(tc.input))
			require.NoError(t, err, "input: %q", tc.input)
			_ = res
		})
	}
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

// TestStripFencesTrailingBrace asserts that stripFences does not over-extend
// when the model appends prose containing a closing brace after the JSON
// (finding 2: LastIndex('}') captured trailing prose braces).
func TestStripFencesTrailingBrace(t *testing.T) {
	// Trailing prose has a brace: LastIndex would pick the prose '}' not the JSON '}'.
	input := `{"nodes":[],"edges":[],"hyperedges":[]} Note: see {config} for details.`
	res, err := ParseFragment("r", []byte(input))
	require.NoError(t, err, "trailing prose with brace must not break parsing")
	require.Empty(t, res.Entities)
}

// TestConceptIDEmptySlug asserts that nodes with punctuation-only labels get
// distinct, non-empty ids rather than colliding on 'concept:<repo>:'
// (finding 3: slugLabel returns ” for labels with no [a-z0-9] runes).
func TestConceptIDEmptySlug(t *testing.T) {
	frag := `{"nodes":[
	  {"id":"n1","label":"!!!","file_type":"concept","source_file":"f.go"},
	  {"id":"n2","label":"???","file_type":"rationale","source_file":"f.go"}
	],"edges":[],"hyperedges":[]}`
	res, err := ParseFragment("repo", []byte(frag))
	require.NoError(t, err)
	require.Len(t, res.Entities, 2)
	ids := []string{res.Entities[0].ID, res.Entities[1].ID}
	// Neither id must be the bare 'concept:repo:' collision value.
	require.NotEqual(t, "concept:repo:", ids[0])
	require.NotEqual(t, "concept:repo:", ids[1])
	// The two distinct labels must produce distinct ids.
	require.NotEqual(t, ids[0], ids[1])
}
