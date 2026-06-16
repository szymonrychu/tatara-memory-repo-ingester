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
	})
	require.Contains(t, got, "Files (chunk 2 of 5):")
	require.Contains(t, got, "- a.go\n- b.go")
	// Placeholders must be fully consumed.
	require.NotContains(t, got, "FILE_LIST")
	require.NotContains(t, got, "CHUNK_NUM")
	require.NotContains(t, got, "TOTAL_CHUNKS")
	// DEEP_MODE is off: the literal token must not leak into the prompt.
	require.NotContains(t, got, "DEEP_MODE (if --mode deep was given)")
}

func TestBuildPromptNoWriteToDiskInstruction(t *testing.T) {
	got := BuildPrompt(PromptVars{FileList: "- a.go", ChunkNum: 1, TotalChunks: 1})
	// Prompt must NOT contain the Write-to-disk instruction that was in the old graphify spec.
	require.NotContains(t, got, "Write tool")
	require.NotContains(t, got, "CHUNK_PATH")
	require.NotContains(t, got, "write the JSON to disk")
	// Must have the explicit return-as-content instruction.
	require.Contains(t, got, "Return the JSON as your message content - do not write to disk, do not use any tool.")
}

func TestBuildPromptKeepsSchemaIntact(t *testing.T) {
	got := BuildPrompt(PromptVars{FileList: "- a.go", ChunkNum: 1, TotalChunks: 1})
	require.Contains(t, got, "Generate the extraction JSON matching this schema exactly:")
	// The schema JSON enumerates semantically_similar_to in the relation pipe-list; the prose
	// line uses backtick quoting (verified by TestExtractionSpecProseUsesBackticks).
	require.Contains(t, got, "semantically_similar_to")
}

// TestExtractionSpecProseUsesBackticks guards the blind spot: the prose description of
// the semantically_similar_to edge must use backticks (verbatim from the graphify source),
// not double quotes. The schema JSON line legitimately uses double quotes, but the
// "add a `semantically_similar_to` edge" sentence must match the source exactly.
func TestExtractionSpecProseUsesBackticks(t *testing.T) {
	require.Contains(t, extractionSpec, "add a `semantically_similar_to` edge",
		"prose line must use backtick-quoted identifier to match graphify extraction-spec source verbatim")
}

// TestBuildPromptIncludesFileContent asserts that the prompt includes actual file
// content (via FILE_CONTENT placeholder) so the tool-less LLM can perform extraction
// against source bytes rather than filenames only (finding 1: content never placed
// in the prompt).
func TestBuildPromptIncludesFileContent(t *testing.T) {
	got := BuildPrompt(PromptVars{
		FileList:    "- auth/session.go",
		FileContent: "// auth/session.go\npackage auth\nfunc ValidateToken() {}",
		ChunkNum:    1,
		TotalChunks: 1,
	})
	// Content must appear verbatim in the rendered prompt.
	require.Contains(t, got, "func ValidateToken() {}")
	// The FILE_CONTENT placeholder must be fully consumed.
	require.NotContains(t, got, "FILE_CONTENT")
}

// TestExtractionSpecContainsFileContentPlaceholder asserts that extraction_spec.txt
// contains the FILE_CONTENT placeholder that BuildPrompt substitutes (finding 1).
func TestExtractionSpecContainsFileContentPlaceholder(t *testing.T) {
	require.Contains(t, extractionSpec, "FILE_CONTENT",
		"extraction_spec.txt must contain FILE_CONTENT placeholder for source bytes")
}

var _ = strings.Contains // ensure import used
