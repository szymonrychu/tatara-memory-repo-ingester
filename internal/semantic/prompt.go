// Package semantic runs the LLM extraction stage: it chunks analyzed files,
// builds the graphify extraction prompt, and maps the returned JSON fragment to
// contract graph types. It is best-effort: callers log and skip on any failure.
package semantic

import (
	_ "embed"
	"strconv"
	"strings"
)

//go:embed extraction_spec.txt
var extractionSpec string

// PromptVars are the placeholder substitutions for the extraction prompt.
type PromptVars struct {
	FileList    string
	ChunkNum    int
	TotalChunks int
}

// BuildPrompt returns the verbatim extraction-spec prompt with FILE_LIST,
// CHUNK_NUM, and TOTAL_CHUNKS substituted. DEEP_MODE is always off.
// Longer tokens are listed first so TOTAL_CHUNKS is not partially matched by CHUNK_NUM.
func BuildPrompt(v PromptVars) string {
	r := strings.NewReplacer(
		"TOTAL_CHUNKS", strconv.Itoa(v.TotalChunks),
		"CHUNK_NUM", strconv.Itoa(v.ChunkNum),
		"FILE_LIST", v.FileList,
	)
	return r.Replace(extractionSpec)
}
