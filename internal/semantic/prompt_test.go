package semantic

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptEmbedsExtractionSpecVerbatim(t *testing.T) {
	// Anchor lines from graphify's extraction-spec prompt block must be present verbatim.
	require.Contains(t, extractionSpec, "You are a graphify extraction subagent. Read the files listed and extract a knowledge graph fragment.")
	require.Contains(t, extractionSpec, "Output ONLY valid JSON matching the schema below - no explanation, no markdown fences, no preamble.")
	require.Contains(t, extractionSpec, "- EXTRACTED: relationship explicit in source (import, call, citation, \"see §3.2\")")
	require.Contains(t, extractionSpec, "Maximum 3 hyperedges per chunk.")
	require.Contains(t, extractionSpec, "confidence_score is REQUIRED on every edge - never omit it, never use 0.5 as a default:")
	require.Contains(t, extractionSpec, "\"nodes\":[{\"id\":\"session_validatetoken\"")
}

func TestBuildPromptSubstitutesPlaceholders(t *testing.T) {
	got := BuildPrompt(PromptVars{
		FileList:    "- a.go\n- b.go",
		ChunkNum:    2,
		TotalChunks: 5,
		ChunkPath:   "/tmp/chunk-2.json",
	})
	require.Contains(t, got, "Files (chunk 2 of 5):")
	require.Contains(t, got, "- a.go\n- b.go")
	require.Contains(t, got, "/tmp/chunk-2.json")
	// Placeholders must be fully consumed.
	require.NotContains(t, got, "FILE_LIST")
	require.NotContains(t, got, "CHUNK_NUM")
	require.NotContains(t, got, "TOTAL_CHUNKS")
	require.NotContains(t, got, "CHUNK_PATH")
	// DEEP_MODE is off: the literal token must not leak into the prompt.
	require.NotContains(t, got, "DEEP_MODE (if --mode deep was given)")
}

func TestBuildPromptKeepsSchemaIntact(t *testing.T) {
	got := BuildPrompt(PromptVars{FileList: "- a.go", ChunkNum: 1, TotalChunks: 1, ChunkPath: "/tmp/c.json"})
	require.Contains(t, got, "Generate the extraction JSON matching this schema exactly:")
	require.Contains(t, got, "\"semantically_similar_to\"")
}

var _ = strings.Contains // ensure import used
