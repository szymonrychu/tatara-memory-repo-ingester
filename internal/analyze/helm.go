package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"text/template/parse"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type helmAnalyzer struct {
	log *slog.Logger
}

// NewHelm returns the Helm chart analyzer.
func NewHelm() Analyzer {
	return helmAnalyzer{log: slog.Default()}
}

func (helmAnalyzer) Name() string { return "helm" }

// Match returns true for Chart.yaml, values.yaml, or any file under a templates/ dir.
func (helmAnalyzer) Match(path string) bool {
	base := filepath.Base(path)
	if base == "Chart.yaml" || base == "values.yaml" {
		return true
	}
	// templates/<file> or <chart>/templates/<file>
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, p := range parts {
		if p == "templates" {
			return true
		}
	}
	return false
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

	var res Result

	for chartRoot, cfiles := range chartFiles {
		// Parse Chart.yaml if present in the file set
		chartYAMLPath := chartRoot + "/Chart.yaml"
		absChartYAML := filepath.Join(repoRoot, chartYAMLPath)

		var manifest chartManifest
		rawChart, err := os.ReadFile(absChartYAML) //nolint:gosec // analyzer reads arbitrary repo files by design
		if err != nil {
			ha.log.Warn("helm: cannot read Chart.yaml", "path", chartYAMLPath, "err", err)
			continue
		}
		if err := sigsyaml.Unmarshal(rawChart, &manifest); err != nil {
			ha.log.Warn("helm: cannot parse Chart.yaml", "path", chartYAMLPath, "err", err)
			continue
		}
		chartName := manifest.Name
		chartID := "helm:chart:" + chartName

		// chartEntityPath is non-empty only when Chart.yaml is in the diff files set.
		// Empty means repo-scoped; tatara-memory exempts empty file_path from push-scope validation.
		chartEntityPath := ""
		for _, f := range cfiles {
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

		// Parse values.yaml if present in the file set
		valuesPath := chartRoot + "/values.yaml"
		valueIDs := map[string]bool{}
		for _, f := range cfiles {
			if filepath.ToSlash(f) == valuesPath {
				absValues := filepath.Join(repoRoot, valuesPath)
				rawValues, err := os.ReadFile(absValues) //nolint:gosec // analyzer reads arbitrary repo files by design
				if err != nil {
					ha.log.Warn("helm: cannot read values.yaml", "path", valuesPath, "err", err)
					break
				}
				var flat map[string]any
				if err := sigsyaml.Unmarshal(rawValues, &flat); err != nil {
					ha.log.Warn("helm: cannot parse values.yaml", "path", valuesPath, "err", err)
					break
				}
				flatKeys := flattenValues(flat, "")
				for _, key := range flatKeys {
					vid := fmt.Sprintf("helm:value:%s.%s", chartName, key)
					valueIDs[key] = true
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

		// Process templates
		for _, f := range cfiles {
			if !isTemplate(f) {
				continue
			}
			ha.processTemplate(repoRoot, f, chartName, valueIDs, &res)
		}
	}

	return res, nil
}

func (ha helmAnalyzer) processTemplate(repoRoot, relPath, chartName string, valueIDs map[string]bool, res *Result) {
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

	// Build a permissive FuncMap so helm builtins don't fail parse
	fm := noopHelmFuncMap()
	t, err := template.New(filepath.Base(relPath)).Funcs(fm).Parse(content)
	if err != nil {
		ha.log.Warn("helm: cannot parse template", "path", relPath, "err", err)
		return
	}

	// Walk the parse tree
	for _, tree := range t.Templates() {
		if tree.Root == nil {
			continue
		}
		walkNodes(tree.Root.Nodes, tmplID, relPath, chartName, valueIDs, res)
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
func walkPipe(pipe *parse.PipeNode, tmplID, srcFile, chartName string, valueIDs map[string]bool, res *Result) {
	if pipe == nil {
		return
	}
	for _, cmd := range pipe.Cmds {
		processCmd(cmd, tmplID, srcFile, chartName, valueIDs, res)
	}
}

// walkNodes recursively walks template parse tree nodes collecting edges.
func walkNodes(nodes []parse.Node, tmplID, srcFile, chartName string, valueIDs map[string]bool, res *Result) {
	for _, n := range nodes {
		switch node := n.(type) {
		case *parse.ActionNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, valueIDs, res)
		case *parse.ListNode:
			if node != nil {
				walkNodes(node.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			}
		case *parse.IfNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, valueIDs, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			}
		case *parse.RangeNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, valueIDs, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			}
		case *parse.WithNode:
			walkPipe(node.Pipe, tmplID, srcFile, chartName, valueIDs, res)
			walkNodes(node.List.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			if node.ElseList != nil {
				walkNodes(node.ElseList.Nodes, tmplID, srcFile, chartName, valueIDs, res)
			}
		case *parse.TemplateNode:
			// {{template "name" .}} -> includes edge
			res.Edges = append(res.Edges, contract.Edge{
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
func processCmd(cmd *parse.CommandNode, tmplID, srcFile, chartName string, valueIDs map[string]bool, res *Result) {
	if len(cmd.Args) == 0 {
		return
	}

	// Check for include/template function call with a string literal first arg
	if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		if (id.Ident == "include" || id.Ident == "template") && len(cmd.Args) >= 2 {
			if strNode, ok := cmd.Args[1].(*parse.StringNode); ok {
				res.Edges = append(res.Edges, contract.Edge{
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
		extractValueRefs(arg, tmplID, srcFile, chartName, valueIDs, res)
	}
}

// extractValueRefs walks a node looking for .Values.* field chains.
func extractValueRefs(n parse.Node, tmplID, srcFile, chartName string, valueIDs map[string]bool, res *Result) {
	switch node := n.(type) {
	case *parse.FieldNode:
		// FieldNode.Ident is a slice of path components, e.g. ["Values","image","repository"]
		if len(node.Ident) >= 2 && node.Ident[0] == "Values" {
			dotted := strings.Join(node.Ident[1:], ".")
			valueKey := fmt.Sprintf("helm:value:%s.%s", chartName, dotted)
			res.Edges = append(res.Edges, contract.Edge{
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
			processCmd(cmd, tmplID, srcFile, chartName, valueIDs, res)
		}
	}
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
		if p == "templates" && i > 0 {
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

// noopHelmFuncMap returns a FuncMap where every helm builtin is a no-op,
// so text/template.Parse doesn't error on unknown function names.
func noopHelmFuncMap() template.FuncMap {
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
}
