package analyze

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

type terraformAnalyzer struct {
	log *slog.Logger
}

// NewTerraform returns an Analyzer for Terraform .tf files.
func NewTerraform() Analyzer {
	return terraformAnalyzer{log: slog.Default()}
}

func (terraformAnalyzer) Name() string { return "terraform" }

func (terraformAnalyzer) Match(path string) bool {
	return strings.HasSuffix(path, ".tf")
}

func (ta terraformAnalyzer) Analyze(_ context.Context, repoRoot string, files []string) (Result, error) {
	var res Result
	parser := hclparse.NewParser()

	for _, relPath := range files {
		absPath := filepath.Join(repoRoot, relPath)
		hclFile, diags := parser.ParseHCLFile(absPath)
		if diags.HasErrors() {
			ta.log.Warn("hcl parse error", "file", relPath, "err", diags.Error())
			continue
		}
		body, ok := hclFile.Body.(*hclsyntax.Body)
		if !ok {
			ta.log.Warn("unexpected body type", "file", relPath)
			continue
		}

		for _, block := range body.Blocks {
			entities, edges, chunks := ta.processBlock(block, relPath)
			res.Entities = append(res.Entities, entities...)
			res.Edges = append(res.Edges, edges...)
			res.Chunks = append(res.Chunks, chunks...)
		}
	}
	return res, nil
}

func (ta terraformAnalyzer) processBlock(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	switch block.Type {
	case "variable":
		return ta.handleVariable(block, relPath)
	case "output":
		return ta.handleOutput(block, relPath)
	case "resource":
		return ta.handleResource(block, relPath)
	case "data":
		return ta.handleData(block, relPath)
	case "module":
		return ta.handleModule(block, relPath)
	}
	return nil, nil, nil
}

func (ta terraformAnalyzer) handleVariable(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	if len(block.Labels) < 1 {
		return nil, nil, nil
	}
	name := block.Labels[0]
	id := "tf:variable:" + name
	ent := contract.Entity{
		ID:       id,
		Name:     name,
		Type:     contract.EntityTFVariable,
		FilePath: relPath,
	}
	chunk := contract.Chunk{
		EntityID: id,
		Type:     contract.EntityTFVariable,
		FilePath: relPath,
		Language: "terraform",
		Header:   fmt.Sprintf("[tf_variable] %s", name),
		Body:     fmt.Sprintf("variable %q { ... }", name),
	}
	return []contract.Entity{ent}, nil, []contract.Chunk{chunk}
}

func (ta terraformAnalyzer) handleOutput(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	if len(block.Labels) < 1 {
		return nil, nil, nil
	}
	name := block.Labels[0]
	id := "tf:output:" + name
	ent := contract.Entity{
		ID:       id,
		Name:     name,
		Type:     contract.EntityTFOutput,
		FilePath: relPath,
	}

	var edges []contract.Edge
	for attrName, attr := range block.Body.Attributes {
		if attrName == "depends_on" {
			edges = append(edges, ta.dependsOnEdges(id, relPath, attr.Expr)...)
		} else {
			edges = append(edges, ta.edgesFromExpr(id, relPath, attr.Expr)...)
		}
	}

	chunk := contract.Chunk{
		EntityID: id,
		Type:     contract.EntityTFOutput,
		FilePath: relPath,
		Language: "terraform",
		Header:   fmt.Sprintf("[tf_output] %s", name),
		Body:     fmt.Sprintf("output %q { ... }", name),
	}
	return []contract.Entity{ent}, edges, []contract.Chunk{chunk}
}

func (ta terraformAnalyzer) handleResource(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	if len(block.Labels) < 2 {
		return nil, nil, nil
	}
	resType, resName := block.Labels[0], block.Labels[1]
	id := fmt.Sprintf("tf:resource:%s.%s", resType, resName)
	ent := contract.Entity{
		ID:       id,
		Name:     resName,
		Type:     contract.EntityTFResource,
		FilePath: relPath,
	}

	edges := ta.edgesFromBody(id, relPath, block.Body)

	chunk := contract.Chunk{
		EntityID: id,
		Type:     contract.EntityTFResource,
		FilePath: relPath,
		Language: "terraform",
		Header:   fmt.Sprintf("[tf_resource] %s.%s", resType, resName),
		Body:     fmt.Sprintf("resource %q %q { ... }", resType, resName),
	}
	return []contract.Entity{ent}, edges, []contract.Chunk{chunk}
}

func (ta terraformAnalyzer) handleData(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	if len(block.Labels) < 2 {
		return nil, nil, nil
	}
	dataType, dataName := block.Labels[0], block.Labels[1]
	id := fmt.Sprintf("tf:data:%s.%s", dataType, dataName)
	ent := contract.Entity{
		ID:       id,
		Name:     dataName,
		Type:     contract.EntityTFData,
		FilePath: relPath,
	}

	edges := ta.edgesFromBody(id, relPath, block.Body)

	chunk := contract.Chunk{
		EntityID: id,
		Type:     contract.EntityTFData,
		FilePath: relPath,
		Language: "terraform",
		Header:   fmt.Sprintf("[tf_data] %s.%s", dataType, dataName),
		Body:     fmt.Sprintf("data %q %q { ... }", dataType, dataName),
	}
	return []contract.Entity{ent}, edges, []contract.Chunk{chunk}
}

func (ta terraformAnalyzer) handleModule(block *hclsyntax.Block, relPath string) ([]contract.Entity, []contract.Edge, []contract.Chunk) {
	if len(block.Labels) < 1 {
		return nil, nil, nil
	}
	name := block.Labels[0]
	id := "tf:module:" + name
	ent := contract.Entity{
		ID:       id,
		Name:     name,
		Type:     contract.EntityTFModule,
		FilePath: relPath,
	}

	var edges []contract.Edge

	// source attribute: emit module_source edge to the literal source string
	if srcAttr, ok := block.Body.Attributes["source"]; ok {
		val, diags := srcAttr.Expr.Value(nil)
		if !diags.HasErrors() && val.Type().Equals(cty.String) {
			src := val.AsString()
			edges = append(edges, contract.Edge{
				From:     id,
				To:       src,
				Relation: contract.RelModuleSource,
				SrcFile:  relPath,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		}
	}

	for attrName, attr := range block.Body.Attributes {
		if attrName == "source" {
			continue
		}
		if attrName == "depends_on" {
			edges = append(edges, ta.dependsOnEdges(id, relPath, attr.Expr)...)
		} else {
			edges = append(edges, ta.edgesFromExpr(id, relPath, attr.Expr)...)
		}
	}

	chunk := contract.Chunk{
		EntityID: id,
		Type:     contract.EntityTFModule,
		FilePath: relPath,
		Language: "terraform",
		Header:   fmt.Sprintf("[tf_module] %s", name),
		Body:     fmt.Sprintf("module %q { ... }", name),
	}
	return []contract.Entity{ent}, edges, []contract.Chunk{chunk}
}

// edgesFromBody walks all attributes in a block body, routing depends_on specially.
func (ta terraformAnalyzer) edgesFromBody(srcID, relPath string, body *hclsyntax.Body) []contract.Edge {
	var edges []contract.Edge
	for attrName, attr := range body.Attributes {
		if attrName == "depends_on" {
			edges = append(edges, ta.dependsOnEdges(srcID, relPath, attr.Expr)...)
		} else {
			edges = append(edges, ta.edgesFromExpr(srcID, relPath, attr.Expr)...)
		}
	}
	return edges
}

// edgesFromExpr collects variable traversals from an expression and maps them to edges.
func (ta terraformAnalyzer) edgesFromExpr(srcID, srcFile string, expr hclsyntax.Expression) []contract.Edge {
	vars := hclsyntax.Variables(expr)
	var edges []contract.Edge
	for _, traversal := range vars {
		root := traversal.RootName()
		if len(traversal) < 2 {
			continue
		}
		attr, ok := traversal[1].(hcl.TraverseAttr)
		if !ok {
			continue
		}
		switch root {
		case "var":
			edges = append(edges, contract.Edge{
				From:     srcID,
				To:       "tf:variable:" + attr.Name,
				Relation: contract.RelVarRef,
				SrcFile:  srcFile,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		case "module":
			edges = append(edges, contract.Edge{
				From:     srcID,
				To:       "tf:module:" + attr.Name,
				Relation: contract.RelReferences,
				SrcFile:  srcFile,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		case "data":
			// data.<type>.<name>.* -> tf:data:<type>.<name>
			if len(traversal) >= 3 {
				nameAttr, ok2 := traversal[2].(hcl.TraverseAttr)
				if ok2 {
					edges = append(edges, contract.Edge{
						From:     srcID,
						To:       fmt.Sprintf("tf:data:%s.%s", attr.Name, nameAttr.Name),
						Relation: contract.RelReferences,
						SrcFile:  srcFile,
						Properties: map[string]string{
							"resolution": contract.ResTypeResolved,
							"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
						},
					})
				}
			}
		case "local", "each", "count", "path", "self", "terraform":
			// built-in meta-references; no graph edge
		default:
			// resource reference: <type>.<name>.*
			edges = append(edges, contract.Edge{
				From:     srcID,
				To:       fmt.Sprintf("tf:resource:%s.%s", root, attr.Name),
				Relation: contract.RelReferences,
				SrcFile:  srcFile,
				Properties: map[string]string{
					"resolution": contract.ResTypeResolved,
					"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
				},
			})
		}
	}
	return edges
}

// dependsOnEdges handles the special depends_on list attribute.
func (ta terraformAnalyzer) dependsOnEdges(srcID, srcFile string, expr hclsyntax.Expression) []contract.Edge {
	vars := hclsyntax.Variables(expr)
	var edges []contract.Edge
	for _, traversal := range vars {
		if len(traversal) < 2 {
			continue
		}
		root := traversal.RootName()
		attr, ok := traversal[1].(hcl.TraverseAttr)
		if !ok {
			continue
		}
		var toID string
		switch root {
		case "module":
			toID = "tf:module:" + attr.Name
		case "data":
			// data.<type>.<name> -> tf:data:<type>.<name>
			if len(traversal) >= 3 {
				nameAttr, ok2 := traversal[2].(hcl.TraverseAttr)
				if ok2 {
					toID = fmt.Sprintf("tf:data:%s.%s", attr.Name, nameAttr.Name)
				}
			}
		case "local", "each", "count", "path", "self", "terraform", "var":
			// built-in or variable references; skip in depends_on context
		default:
			toID = fmt.Sprintf("tf:resource:%s.%s", root, attr.Name)
		}
		if toID == "" {
			continue
		}
		edges = append(edges, contract.Edge{
			From:     srcID,
			To:       toID,
			Relation: contract.RelDependsOn,
			SrcFile:  srcFile,
			Properties: map[string]string{
				"resolution": contract.ResTypeResolved,
				"confidence": contract.ConfidenceFor(contract.ResTypeResolved),
			},
		})
	}
	return edges
}
