package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type helmAnalyzer struct {
	log            *slog.Logger
	repoRoot       string          // optional; when set, Match validates Chart.yaml presence on disk
	chartRootCache map[string]bool // memoizes chartRoot->Chart.yaml-exists results (finding 2 r2)
}

// NewHelm returns the Helm chart analyzer. repoRoot is optional (empty = no disk validation in Match).
func NewHelm(repoRoot string) Analyzer {
	return helmAnalyzer{log: slog.Default(), repoRoot: repoRoot, chartRootCache: map[string]bool{}}
}

func (helmAnalyzer) Name() string { return "helm" }

// Match returns true for Chart.yaml, or values.yaml/templates/ files that belong to a real
// Helm chart (has a Chart.yaml sibling on disk when repoRoot is set).
func (ha helmAnalyzer) Match(filePath string) bool {
	base := filepath.Base(filePath)

	// Chart.yaml is the chart marker itself; always claim it.
	if base == "Chart.yaml" {
		return true
	}

	if ha.repoRoot == "" {
		// No disk validation available: claim on basename alone (legacy/test mode).
		if base == "values.yaml" {
			return true
		}
		parts := strings.Split(filepath.ToSlash(filePath), "/")
		for _, p := range parts {
			if p == "templates" {
				return true
			}
		}
		return false
	}

	// With repoRoot set, require a Chart.yaml sibling for values.yaml and templates/ files.
	chartRoot := findChartRoot(filePath)
	return ha.chartRootHasChartYAML(chartRoot)
}

// chartRootHasChartYAML checks whether chartRoot contains Chart.yaml, memoizing the result.
func (ha helmAnalyzer) chartRootHasChartYAML(chartRoot string) bool {
	if cached, ok := ha.chartRootCache[chartRoot]; ok {
		return cached
	}
	chartYAMLPath := path.Join(chartRoot, "Chart.yaml")
	absChartYAML := filepath.Join(ha.repoRoot, filepath.FromSlash(chartYAMLPath))
	_, err := os.Stat(absChartYAML)
	exists := err == nil
	ha.chartRootCache[chartRoot] = exists
	return exists
}

// chartManifest is the subset of Chart.yaml we need.
type chartManifest struct {
	Name         string `json:"name"`
	Dependencies []struct {
		Name string `json:"name"`
	} `json:"dependencies"`
}

func (ha helmAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	// Group files by chart root (dir containing Chart.yaml).
	chartFiles := map[string][]string{}

	for _, f := range files {
		cr := findChartRoot(f)
		chartFiles[cr] = append(chartFiles[cr], f)
	}

	// Sort chart roots for deterministic output (finding 10).
	chartRoots := make([]string, 0, len(chartFiles))
	for cr := range chartFiles {
		chartRoots = append(chartRoots, cr)
	}
	sort.Strings(chartRoots)

	var res Result

	for _, chartRoot := range chartRoots {
		cfiles := chartFiles[chartRoot]
		// Parse Chart.yaml if present in the file set.
		// path.Join normalises "."+"/Chart.yaml" -> "Chart.yaml" for root charts (finding 1).
		chartYAMLPath := path.Join(chartRoot, "Chart.yaml")
		absChartYAML := filepath.Join(repoRoot, filepath.FromSlash(chartYAMLPath))

		var manifest chartManifest
		rawChart, err := os.ReadFile(absChartYAML) //nolint:gosec // analyzer reads arbitrary repo files by design
		if err != nil {
			ha.log.Warn("helm: cannot read Chart.yaml", "path", chartYAMLPath, "err", err)
			continue
		}
		if err := sigsyaml.Unmarshal(rawChart, &manifest); err != nil {
			ha.log.Warn("helm: cannot parse Chart.yaml", "path", chartYAMLPath, "err", err)
			res.ParseErrors++
			continue
		}
		chartName := manifest.Name
		// Guard against Chart.yaml files with no name field (finding 8).
		if chartName == "" {
			ha.log.Warn("helm: Chart.yaml missing name", "path", chartYAMLPath)
			continue
		}
		chartID := "helm:chart:" + chartName

		// chartEntityPath is non-empty only when Chart.yaml is in the diff files set.
		// Empty means repo-scoped; tatara-memory exempts empty file_path from push-scope validation.
		chartEntityPath := ""
		for _, f := range cfiles {
			// path.Join normalises for root-chart match (finding 1).
			if filepath.ToSlash(f) == chartYAMLPath {
				chartEntityPath = chartYAMLPath
				break
			}
		}

		// Emit helm_chart entity
		res.Entities = append(res.Entities, contract.Entity{
			ID:       chartID,
			Name:     chartName,
			Type:     contract.EntityHelmChart,
			FilePath: chartEntityPath,
		})

		// Subchart edges are sourced from Chart.yaml. Emit only when Chart.yaml is
		// in the diff: unlike entities, the server does NOT exempt an empty edge
		// src_file (every edge src_file must be in the pushed files), and an
		// unchanged Chart.yaml means the dependency relationships are unchanged.
		if chartEntityPath != "" {
			for _, dep := range manifest.Dependencies {
				res.Edges = append(res.Edges, contract.Edge{
					From:     chartID,
					To:       "helm:chart:" + dep.Name,
					Relation: contract.RelSubchart,
					SrcFile:  chartEntityPath,
					Properties: map[string]string{
						"resolution": contract.ResTypeResolved,
						"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
					},
				})
			}
		}

		// Parse values.yaml if present in the file set.
		// path.Join normalises for root-chart match (finding 1).
		valuesPath := path.Join(chartRoot, "values.yaml")
		for _, f := range cfiles {
			if filepath.ToSlash(f) == valuesPath {
				absValues := filepath.Join(repoRoot, filepath.FromSlash(valuesPath))
				rawValues, err := os.ReadFile(absValues) //nolint:gosec // analyzer reads arbitrary repo files by design
				if err != nil {
					ha.log.Warn("helm: cannot read values.yaml", "path", valuesPath, "err", err)
					break
				}
				var flat map[string]any
				if err := sigsyaml.Unmarshal(rawValues, &flat); err != nil {
					ha.log.Warn("helm: cannot parse values.yaml", "path", valuesPath, "err", err)
					res.ParseErrors++
					break
				}
				// Sort keys for deterministic output (finding 10).
				flatKeys := flattenValues(flat, "")
				sort.Strings(flatKeys)
				for _, key := range flatKeys {
					vid := fmt.Sprintf("helm:value:%s.%s", chartName, key)
					res.Entities = append(res.Entities, contract.Entity{
						ID:       vid,
						Name:     key,
						Type:     contract.EntityHelmValue,
						FilePath: valuesPath,
					})
				}
				break
			}
		}

		// Process templates; valueIDs removed (was dead parameter - finding 4/6).
		for _, f := range cfiles {
			if !isTemplate(f) {
				continue
			}
			ha.processTemplate(repoRoot, f, chartName, &res)
		}
	}

	return res, nil
}

func (ha helmAnalyzer) processTemplate(repoRoot, relPath, chartName string, res *Result) {
	tmplID := "helm:template:" + relPath

	res.Entities = append(res.Entities, contract.Entity{
		ID:       tmplID,
		Name:     filepath.Base(relPath),
		Type:     contract.EntityHelmTemplate,
		FilePath: relPath,
	})

	absPath := filepath.Join(repoRoot, relPath)
	raw, err := os.ReadFile(absPath) //nolint:gosec // analyzer reads arbitrary repo files by design
	if err != nil {
		ha.log.Warn("helm: cannot read template", "path", relPath, "err", err)
		return
	}

	content := string(raw)

	// Use package-level FuncMap (built once, finding 9).
	fm := helmFuncMap()
	t, err := template.New(filepath.Base(relPath)).Funcs(fm).Parse(content)
	if err != nil {
		ha.log.Warn("helm: cannot parse template", "path", relPath, "err", err)
		res.ParseErrors++
		return
	}

	// seen deduplicates edges per template (finding 5): key = relation+from+to.
	seen := map[string]bool{}

	// Walk the parse tree
	for _, tree := range t.Templates() {
		if tree.Root == nil {
			continue
		}
		walkNodes(tree.Root.Nodes, tmplID, relPath, chartName, seen, res)
	}

	res.Chunks = append(res.Chunks, contract.Chunk{
		EntityID: tmplID,
		Type:     contract.EntityHelmTemplate,
		FilePath: relPath,
		Language: "helm",
		Header:   fmt.Sprintf("[helm_template] %s", relPath),
		Body:     content,
	})
}

// walkPipe processes all commands in a PipeNode, extracting value refs and include edges.
func walkPipe(pipe *parse.PipeNode, tmplID, srcFile, chartName string, seen map[string]bool, res *Result) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.Cmds {
		processCmd(cmd, tmplID, srcFile, chartName, seen, res)
	}
}

// walkNodes recursively walks template parse tree nodes collecting edges.
func walkNodes(nodes []parse.Node, tmplID, srcFile, chartName string, seen map[string]bool, res *Result) {
	for _, n := range nodes {
		switch node := n.(type) {
		case *parse.ActionNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, seen, res)
		case *parse.ListNode:
			if node != nil {
				walkNodes(node.Nodes, tmplID, srcFile, chartName, seen, res)
			}
		case *parse.IfNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, seen, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, seen, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, seen, res)
			}
		case *parse.RangeNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, seen, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, seen, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, seen, res)
			}
		case *parse.WithNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, seen, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, seen, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, seen, res)
			}
		case *parse.TemplateNode:
			// {{template "name" .}} -> includes edge
			appendEdgeOnce(seen, res, contract.Edge{
				From:     tmplID,
				To:       "helm:include:" + node.Name,
				Relation: contract.RelIncludes,
				SrcFile:  srcFile,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		}
	}
}

// processCmd handles a single command node - extracts field chains and include calls.
func processCmd(cmd *parse.CommandNode, tmplID, srcFile, chartName string, seen map[string]bool, res *Result) {
	if len(cmd.Args) == 0 {
		return
	}

	// Check for include/template function call with a string literal first arg
	if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		if (id.Ident == "include" || id.Ident == "template") && len(cmd.Args) >= 2 {
			if strNode, ok := cmd.Args[1].(*parse.StringNode); ok {
				appendEdgeOnce(seen, res, contract.Edge{
					From:     tmplID,
					To:       "helm:include:" + strNode.Text,
					Relation: contract.RelIncludes,
					SrcFile:  srcFile,
					Properties: map[string]string{
						"resolution": contract.ResTypeResolved,
						"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
					},
				})
			}
		}
	}

	// Check for .Values.* field chains
	for _, arg := range cmd.Args {
		extractValueRefs(arg, tmplID, srcFile, chartName, seen, res)
	}
}

// extractValueRefs walks a node looking for .Values.* field chains.
// NOTE: helm:include: and helm:value: edge targets are emitted without a corresponding
// entity in this push when values.yaml / the define source is not in the diff.
// The tatara-memory server upserts placeholder nodes for dangling edge To targets,
// so these edges are valid on incremental ingests (finding 7).
func extractValueRefs(n parse.Node, tmplID, srcFile, chartName string, seen map[string]bool, res *Result) {
	switch node := n.(type) {
	case *parse.FieldNode:
		// FieldNode.Ident is a slice of path components, e.g. ["Values","image","repository"]
		if len(node.Ident) >= 2 && node.Ident[0] == "Values" {
			dotted := strings.Join(node.Ident[1:], ".")
			valueKey := fmt.Sprintf("helm:value:%s.%s", chartName, dotted)
			appendEdgeOnce(seen, res, contract.Edge{
				From:     tmplID,
				To:       valueKey,
				Relation: contract.RelValueRef,
				SrcFile:  srcFile,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		}
	case *parse.PipeNode:
		for _, cmd := range node.Cmds {
			processCmd(cmd, tmplID, srcFile, chartName, seen, res)
		}
	}
}

// appendEdgeOnce appends e to res.Edges only if it has not been seen before (finding 5).
func appendEdgeOnce(seen map[string]bool, res *Result, e contract.Edge) {
	k := e.Relation + "|" + e.From + "|" + e.To
	if seen[k] {
		return
	}
	seen[k] = true
	res.Edges = append(res.Edges, e)
}

// flattenValues flattens a nested map into dotted keys, skipping non-scalar leaves.
func flattenValues(m map[string]any, prefix string) []string {
	var keys []string
	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		switch child := v.(type) {
		case map[string]any:
			keys = append(keys, flattenValues(child, full)...)
		default:
			keys = append(keys, full)
		}
	}
	return keys
}

// findChartRoot returns the directory component that is the chart root
// (i.e. the parent of templates/ or the dir of Chart.yaml/values.yaml).
func findChartRoot(relPath string) string {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for i, p := range parts {
		if p == "templates" {
			// Fix finding 3: a top-level templates/ dir (i==0) means chart root is ".".
			if i == 0 {
				return "."
			}
			return strings.Join(parts[:i], "/")
		}
	}
	// Chart.yaml or values.yaml: parent dir is the chart root
	if len(parts) > 1 {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return "."
}

// isTemplate reports whether a path is under a templates/ directory.
func isTemplate(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, p := range parts {
		if p == "templates" {
			return true
		}
	}
	return false
}

// helmFuncMap is a permissive FuncMap where every helm builtin is a no-op.
// Built once at package init (finding 9) and reused across all processTemplate calls.
var helmFuncMap = sync.OnceValue(func() template.FuncMap {
	noop := func(args ...any) string { return "" }
	noopBool := func(args ...any) bool { return false }
	names := []string{
		"include", "toYaml", "fromYaml", "toJson", "fromJson",
		"required", "default", "empty", "coalesce", "compact",
		"toRawJson", "toPrettyJson", "b64enc", "b64dec",
		"quote", "squote", "upper", "lower", "title", "untitle",
		"trim", "trimAll", "trimPrefix", "trimSuffix",
		"contains", "hasPrefix", "hasSuffix", "replace",
		"cat", "indent", "nindent", "wrap", "wrapWith",
		"list", "dict", "set", "unset", "hasKey", "keys", "values",
		"merge", "mergeOverwrite", "pick", "omit",
		"toStrings", "toString", "int", "int64", "float64",
		"add", "sub", "div", "mul", "mod", "max", "min",
		"ceil", "floor", "round",
		"now", "date", "dateModify", "dateInZone", "toDate",
		"uuidv4", "sha256sum", "sha1sum", "adler32sum",
		"htpasswd", "encryptAES", "decryptAES",
		"kindOf", "kindIs", "typeOf", "typeIs", "typeIsLike",
		"deepEqual", "deepCopy",
		"semver", "semverCompare",
		"lookup", "fail",
		"tpl", "template",
	}
	fm := template.FuncMap{}
	for _, name := range names {
		fm[name] = noop
	}
	fm["and"] = noopBool
	fm["or"] = noopBool
	fm["not"] = noopBool
	fm["eq"] = noopBool
	fm["ne"] = noopBool
	fm["lt"] = noopBool
	fm["le"] = noopBool
	fm["gt"] = noopBool
	fm["ge"] = noopBool
	return fm
})
