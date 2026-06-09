package semantic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// rawFragment mirrors graphify's extraction JSON schema.
type rawFragment struct {
	Nodes []struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		FileType   string `json:"file_type"`
		SourceFile string `json:"source_file"`
		SourceURL  string `json:"source_url"`
		CapturedAt string `json:"captured_at"`
		Author     string `json:"author"`
	} `json:"nodes"`
	Edges []struct {
		Source          string  `json:"source"`
		Target          string  `json:"target"`
		Relation        string  `json:"relation"`
		Confidence      string  `json:"confidence"`
		ConfidenceScore float64 `json:"confidence_score"`
		SourceFile      string  `json:"source_file"`
	} `json:"edges"`
	Hyperedges []struct {
		ID              string   `json:"id"`
		Label           string   `json:"label"`
		Nodes           []string `json:"nodes"`
		Relation        string   `json:"relation"`
		ConfidenceScore float64  `json:"confidence_score"`
		SourceFile      string   `json:"source_file"`
	} `json:"hyperedges"`
}

// maxHyperedgesPerChunk matches the extraction-spec cap.
const maxHyperedgesPerChunk = 3

// ParseFragment maps a graphify extraction JSON fragment to contract graph
// types for one repo. Concept/rationale nodes become Entities with deterministic
// ids (concept:<repo>:<slug>); code/document/paper/image nodes reference AST
// entity ids and are not re-emitted. Edges carry the semantic relation, score,
// and tier. Hyperedges are capped at 3 with deterministic ids.
func ParseFragment(repo string, body []byte) (analyze.Result, error) {
	var f rawFragment
	if err := json.Unmarshal(stripFences(body), &f); err != nil {
		return analyze.Result{}, fmt.Errorf("parse extraction fragment: %w", err)
	}
	// remap: model-emitted id -> canonical conceptID for concept/rationale nodes.
	// This prevents dangling edges when the LLM uses its own node ids in edge
	// endpoints rather than the re-keyed concept:<repo>:<slug> form.
	remap := map[string]string{}
	var res analyze.Result
	for _, n := range f.Nodes {
		typ := entityTypeFor(n.FileType)
		if typ == "" {
			continue // code/document/paper/image: references an AST node, not re-emitted
		}
		cid := conceptID(repo, n.Label)
		remap[n.ID] = cid
		res.Entities = append(res.Entities, contract.Entity{
			ID:         cid,
			Name:       n.Label,
			Type:       typ,
			FilePath:   n.SourceFile,
			SourceURL:  n.SourceURL,
			Author:     n.Author,
			CapturedAt: n.CapturedAt,
		})
	}
	remapID := func(id string) string {
		if mapped, ok := remap[id]; ok {
			return mapped
		}
		return id
	}
	for _, e := range f.Edges {
		res.Edges = append(res.Edges, contract.Edge{
			From:            remapID(e.Source),
			To:              remapID(e.Target),
			Relation:        e.Relation,
			SrcFile:         e.SourceFile,
			ConfidenceScore: e.ConfidenceScore,
			ConfidenceTier:  contract.TierForScore(e.ConfidenceScore),
		})
	}
	for i, h := range f.Hyperedges {
		if i >= maxHyperedgesPerChunk {
			break
		}
		res.Hyperedges = append(res.Hyperedges, contract.Hyperedge{
			ID:              fmt.Sprintf("he:%s:%s:%s", repo, h.SourceFile, h.ID),
			Label:           h.Label,
			Relation:        h.Relation,
			ConfidenceScore: h.ConfidenceScore,
			SrcFile:         h.SourceFile,
			Members:         h.Nodes,
		})
	}
	return res, nil
}

// entityTypeFor maps a graphify file_type to a contract concept/rationale entity
// type, or "" for node types that reference an existing AST entity.
func entityTypeFor(fileType string) string {
	switch fileType {
	case "concept":
		return contract.EntityConcept
	case "rationale":
		return contract.EntityRationale
	default:
		return ""
	}
}

// conceptID is the deterministic id for a concept/rationale node: a slug of the
// label scoped to the repo. Re-extraction of the same label upserts, not dupes.
func conceptID(repo, label string) string {
	return "concept:" + repo + ":" + slugLabel(label)
}

// slugLabel lowercases a label and collapses runs of non-[a-z0-9] into single
// hyphens, trimming leading/trailing hyphens.
func slugLabel(label string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(label) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// stripFences removes a leading ```json / ``` fence and a trailing ``` fence if
// the model wrapped its JSON despite instructions to the contrary.
func stripFences(body []byte) []byte {
	s := strings.TrimSpace(string(body))
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return []byte(strings.TrimSpace(s))
}
