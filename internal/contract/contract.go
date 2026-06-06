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
	Properties  map[string]string `json:"properties,omitempty"`
}

// Edge is a directed, typed relationship between two entities.
type Edge struct {
	From       string            `json:"from"`
	To         string            `json:"to"`
	Relation   string            `json:"relation"`
	SrcFile    string            `json:"src_file"`
	Properties map[string]string `json:"properties,omitempty"`
}

// GraphPush is one /code-graph:bulk request.
type GraphPush struct {
	Repo     string   `json:"repo"`
	Commit   string   `json:"commit,omitempty"`
	Files    []string `json:"files"`
	Entities []Entity `json:"entities"`
	Edges    []Edge   `json:"edges"`
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
