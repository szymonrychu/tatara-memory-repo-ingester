// Package contract holds the wire types and vocabulary shared with
// tatara-memory's codegraph and memory APIs. These types mirror the server's
// JSON shapes byte-for-byte; contract_shape_test.go guards against drift.
package contract

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

// GraphPush is one /code-graph:bulk request.
type GraphPush struct {
	Repo       string      `json:"repo"`
	Commit     string      `json:"commit,omitempty"`
	Files      []string    `json:"files"`
	Entities   []Entity    `json:"entities"`
	Edges      []Edge      `json:"edges"`
	Symbols    []SymbolRow `json:"symbols,omitempty"`
	Hyperedges []Hyperedge `json:"hyperedges,omitempty"` // empty until Phase 2
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

// IngestJob is the /memories:bulk and /ingest-jobs/{id} response.
type IngestJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
	Failed int    `json:"failed"`
	Errors []struct {
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

// ConfidenceFor returns the prior confidence string for a resolution level.
func ConfidenceFor(resolution string) string {
	switch resolution {
	case ResTypeResolved:
		return "0.98"
	case ResScopedNameMatch:
		return "0.85"
	case ResImportedNameMatch:
		return "0.7"
	case ResGlobalNameMatch:
		return "0.45"
	case ResAmbiguousMultiDef:
		return "0.2"
	default:
		return "0.0"
	}
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
