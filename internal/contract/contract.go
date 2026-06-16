// Package contract holds the wire types and vocabulary shared with
// tatara-memory's codegraph and memory APIs. These types mirror the server's
// JSON shapes byte-for-byte; contract_shape_test.go guards against drift.
package contract

import "strconv"

// Entity is a node in the code graph.
type Entity struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	FilePath    string            `json:"file_path"`
	LineStart   int               `json:"line_start,omitempty"`  // Go already computes these
	LineEnd     int               `json:"line_end,omitempty"`    //
	SourceURL   string            `json:"source_url,omitempty"`  // doc frontmatter
	Author      string            `json:"author,omitempty"`      // doc frontmatter
	CapturedAt  string            `json:"captured_at,omitempty"` // doc frontmatter, RFC3339
	Properties  map[string]string `json:"properties,omitempty"`
}

// Edge is a directed, typed relationship between two entities.
type Edge struct {
	From            string            `json:"from"`
	To              string            `json:"to"`
	Relation        string            `json:"relation"`
	SrcFile         string            `json:"src_file"`
	ConfidenceScore float64           `json:"confidence_score,omitempty"` // 1.0 EXTRACTED, <1 INFERRED
	ConfidenceTier  string            `json:"confidence_tier,omitempty"`  // EXTRACTED|INFERRED|AMBIGUOUS
	Properties      map[string]string `json:"properties,omitempty"`
}

// Symbol roles.
const (
	RoleProvides = "provides"
	RoleRequires = "requires"
)

// SymbolRow is one cross-repo symbol emission (mirrors tatara-memory's SymbolRow).
type SymbolRow struct {
	Symbol   string `json:"symbol"`
	Lang     string `json:"lang"`
	Kind     string `json:"kind"`
	Role     string `json:"role"`
	EntityID string `json:"entity_id"`
	SrcFile  string `json:"src_file"`
}

// Hyperedge is an n-ary relationship over 3+ entities (Phase 2 producer; reserved now).
type Hyperedge struct {
	ID              string            `json:"id"`
	Label           string            `json:"label"`
	Relation        string            `json:"relation"` // participate_in|implement|form
	ConfidenceScore float64           `json:"confidence_score,omitempty"`
	SrcFile         string            `json:"src_file"`
	Members         []string          `json:"members"` // entity IDs (3+)
	Properties      map[string]string `json:"properties,omitempty"`
}

// GraphPush is one /code-graph:bulk request. Extractor tags the origin of every
// row in this push ("" or "ast" for the AST extractor, "semantic" for the LLM
// stage); reconcile scopes its per-src_file deletes by it so the two origins do
// not clobber each other. FileSHAs (path->content_sha) is set on the semantic
// push to update the server's extraction cache.
type GraphPush struct {
	Repo       string            `json:"repo"`
	Commit     string            `json:"commit,omitempty"`
	Extractor  string            `json:"extractor,omitempty"`
	Files      []string          `json:"files"`
	Entities   []Entity          `json:"entities"`
	Edges      []Edge            `json:"edges"`
	Symbols    []SymbolRow       `json:"symbols,omitempty"`
	Hyperedges []Hyperedge       `json:"hyperedges,omitempty"`
	FileSHAs   map[string]string `json:"file_shas,omitempty"`
}

// Extractor origin tags written onto every row of a GraphPush. The server
// stores the extractor as a free string and scopes reconcile deletes by it, so
// all three values are treated as distinct, recognised origins. ExtractorAST and
// ExtractorSemantic are also declared in tatara-memory codegraph/types.go;
// ExtractorSCIP is ingester-only (SCIP index source) and is accepted by the
// server under the same free-string contract. If the server ever validates
// against an allowlist, add ExtractorSCIP there too.
const (
	ExtractorAST      = "ast"
	ExtractorSemantic = "semantic"
	ExtractorSCIP     = "scip"
)

// FileSHA pairs a repo-relative path with the sha256 of its working-tree content.
type FileSHA struct {
	Path       string `json:"path"`
	ContentSHA string `json:"content_sha"`
}

// SemanticMissesRequest is the POST /code-graph/semantic-misses body: the set of
// analyzed files with their current content_sha. The server returns the subset
// whose stored content_sha differs or is absent (cache miss -> needs extraction).
type SemanticMissesRequest struct {
	Repo  string    `json:"repo"`
	Files []FileSHA `json:"files"`
}

// PushResult is the /code-graph:bulk response.
type PushResult struct {
	Repo             string `json:"repo"`
	Files            int    `json:"files"`
	EntitiesUpserted int    `json:"entities_upserted"`
	EdgesUpserted    int    `json:"edges_upserted"`
}

// IngestItem is one /memories:bulk item.
type IngestItem struct {
	IdempotencyKey string            `json:"idempotency_key"`
	Text           string            `json:"text"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// BulkMemoriesRequest is the /memories:bulk request body. Repo is required by
// the server when ReconcileFiles is non-empty (it scopes the per-file purge).
// ReconcileFiles, when set, instructs the server to purge prior memories for
// each file (by source) before inserting Items. Absent ReconcileFiles preserves
// insert-only behavior.
type BulkMemoriesRequest struct {
	Repo           string       `json:"repo,omitempty"`
	ReconcileFiles []string     `json:"reconcile_files,omitempty"`
	Items          []IngestItem `json:"items"`
}

// IngestJob is the /memories:bulk and /ingest-jobs/{id} response.
type IngestJob struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	CreatedAt string `json:"created_at,omitempty"` // RFC3339; server-assigned
	UpdatedAt string `json:"updated_at,omitempty"` // RFC3339; server-assigned
	Errors    []struct {
		IdempotencyKey string `json:"idempotency_key"`
		Error          string `json:"error"`
	} `json:"errors,omitempty"`
}

// Terminal reports whether a job status is final.
func (j IngestJob) Terminal() bool {
	switch j.Status {
	case JobSucceeded, JobFailed, JobPartial:
		return true
	}
	return false
}

// Chunk is an analyzer's semantic-chunk output (assembled into an IngestItem by push).
type Chunk struct {
	EntityID string
	Type     string
	FilePath string
	Language string
	Header   string
	Body     string
}

// Job status values.
const (
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobPartial   = "partial"
)

// Entity types.
const (
	EntityRepo         = "repo"
	EntityFile         = "file"
	EntityGoPackage    = "go_package"
	EntityGoType       = "go_type"
	EntityGoFunc       = "go_func"
	EntityGoMethod     = "go_method"
	EntityPyModule     = "py_module"
	EntityPyClass      = "py_class"
	EntityPyFunc       = "py_func"
	EntityJSModule     = "js_module"
	EntityJSClass      = "js_class"
	EntityJSFunc       = "js_func"
	EntityTFResource   = "tf_resource"
	EntityTFData       = "tf_data"
	EntityTFModule     = "tf_module"
	EntityTFVariable   = "tf_variable"
	EntityTFOutput     = "tf_output"
	EntityHelmChart    = "helm_chart"
	EntityHelmTemplate = "helm_template"
	EntityHelmValue    = "helm_value"
	EntityDocFile      = "doc_file"
	EntityDocSection   = "doc_section"
	EntityConcept      = "concept"
	EntityRationale    = "rationale"
)

// Edge relations.
const (
	RelContains     = "contains"
	RelDefines      = "defines"
	RelImports      = "imports"
	RelCalls        = "calls"
	RelReferences   = "references"
	RelImplements   = "implements"
	RelDependsOn    = "depends_on"
	RelModuleSource = "module_source"
	RelVarRef       = "var_ref"
	RelOutputRef    = "output_ref"
	RelValueRef     = "value_ref"
	RelIncludes     = "includes"
	RelSubchart     = "subchart"

	// Semantic relations (reserved Phase 0, emitted Phase 2).
	RelConceptuallyRelated = "conceptually_related_to"
	RelSemanticallySimilar = "semantically_similar_to"
	RelRationaleFor        = "rationale_for"
	RelSharesDataWith      = "shares_data_with"
	RelCites               = "cites"
)

// M3 call-edge resolution levels (property key "resolution").
const (
	ResTypeResolved      = "type_resolved"
	ResScopedNameMatch   = "scoped_name_match"
	ResImportedNameMatch = "imported_name_match"
	ResGlobalNameMatch   = "global_name_match"
	ResAmbiguousMultiDef = "ambiguous_multi_def"
	ResUnresolved        = "unresolved"
)

// ConfidenceScoreFor returns the prior confidence float64 for a resolution level.
// This is the single source of truth; ConfidenceFor derives from it.
func ConfidenceScoreFor(resolution string) float64 {
	switch resolution {
	case ResTypeResolved:
		return 0.98
	case ResScopedNameMatch:
		return 0.85
	case ResImportedNameMatch:
		return 0.7
	case ResGlobalNameMatch:
		return 0.45
	case ResAmbiguousMultiDef:
		return 0.2
	default:
		return 0.0
	}
}

// ConfidenceFor returns the prior confidence string for a resolution level.
func ConfidenceFor(resolution string) string {
	s := strconv.FormatFloat(ConfidenceScoreFor(resolution), 'f', -1, 64)
	return s
}

// Confidence tier values (typed column on code_edges; promoted from the scalar score).
const (
	TierExtracted = "EXTRACTED"
	TierInferred  = "INFERRED"
	TierAmbiguous = "AMBIGUOUS"
)

// TierForScore maps a confidence score to a tier:
// 1.0 -> EXTRACTED; (0.3,1.0) -> INFERRED; <=0.3 -> AMBIGUOUS.
func TierForScore(score float64) string {
	switch {
	case score >= 1.0:
		return TierExtracted
	case score > 0.3:
		return TierInferred
	default:
		return TierAmbiguous
	}
}
