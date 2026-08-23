package semantic

import (
	"bytes"
	"crypto/sha256"
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
//
// validFiles is the set of paths that were actually sent to the LLM. Any edge
// whose source_file is absent from validFiles is dropped (the memory server
// rejects the whole push otherwise). Any concept/rationale entity whose
// source_file is absent has its FilePath blanked so the server treats it as
// repo-scoped rather than rejecting the push. Pass nil to skip validation
// (all paths accepted as-is).
func ParseFragment(repo string, body []byte, validFiles map[string]struct{}) (analyze.Result, error) {
	var f rawFragment
	dec := json.NewDecoder(bytes.NewReader(stripFences(body)))
	if err := dec.Decode(&f); err != nil {
		return analyze.Result{}, fmt.Errorf("parse extraction fragment: %w", err)
	}
	// inValidFiles returns true when validFiles is nil (skip validation,
	// including for an empty path) or the path is present in the set.
	inValidFiles := func(path string) bool {
		if validFiles == nil {
			return true
		}
		_, ok := validFiles[path]
		return ok
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
		fp := n.SourceFile
		if fp != "" && !inValidFiles(fp) {
			fp = "" // blank hallucinated paths; server treats "" as repo-scoped
		}
		res.Entities = append(res.Entities, contract.Entity{
			ID:         cid,
			Name:       n.Label,
			Type:       typ,
			FilePath:   fp,
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
		from := remapID(e.Source)
		to := remapID(e.Target)
		// Drop edges with empty or self endpoints; they reference non-existent entities.
		if from == "" || to == "" || from == to {
			continue
		}
		// Drop edges whose source_file is not in the chunk's file set; the memory
		// server rejects the entire push if any edge.SrcFile is not in Files.
		if !inValidFiles(e.SourceFile) {
			continue
		}
		res.Edges = append(res.Edges, contract.Edge{
			From:            from,
			To:              to,
			Relation:        e.Relation,
			SrcFile:         e.SourceFile,
			ConfidenceScore: e.ConfidenceScore,
			ConfidenceTier:  contract.TierForScore(e.ConfidenceScore),
		})
	}
	emitted := 0
	for _, h := range f.Hyperedges {
		if emitted >= maxHyperedgesPerChunk {
			break
		}
		// Drop hyperedges whose source_file is not in the chunk's file set: like
		// edges, the memory server rejects the entire push if any src_file is not
		// in Files (mirrors the edge guard above; finding 2).
		if !inValidFiles(h.SourceFile) {
			continue
		}
		// Remap members through the concept-id table, mirroring edge endpoint
		// remapping, then drop empty ids and dedup preserving first-seen order.
		// remapID is keyed on the slugified Label, not the node id, so distinct
		// model node ids (or a literal duplicate id) can collapse to the same
		// member here. The server counts DISTINCT members against the 3+
		// contract, so the arity check below must run on this deduped slice,
		// not len(h.Nodes) - checking the raw node count would let a hyperedge
		// through that the server then rejects, discarding the whole batch.
		seen := make(map[string]struct{}, len(h.Nodes))
		members := make([]string, 0, len(h.Nodes))
		for _, m := range h.Nodes {
			rm := remapID(m)
			if rm == "" {
				continue
			}
			if _, ok := seen[rm]; ok {
				continue
			}
			seen[rm] = struct{}{}
			members = append(members, rm)
		}
		if len(members) < 3 {
			continue
		}
		res.Hyperedges = append(res.Hyperedges, contract.Hyperedge{
			ID:              hyperedgeID(repo, h.SourceFile, h.ID, h.Label, members),
			Label:           h.Label,
			Relation:        h.Relation,
			ConfidenceScore: h.ConfidenceScore,
			SrcFile:         h.SourceFile,
			Members:         members,
		})
		emitted++
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
// When the label has no alphanumeric runes, a stable sha256 hash of the raw label
// is used so distinct punctuation-only labels get distinct ids (mirroring hyperedgeID).
func conceptID(repo, label string) string {
	slug := slugLabel(label)
	if slug == "" {
		h := sha256.New()
		h.Write([]byte(label))
		return fmt.Sprintf("concept:%s:%x", repo, h.Sum(nil)[:8])
	}
	return "concept:" + repo + ":" + slug
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

// hyperedgeID builds a deterministic id for a hyperedge. Source file and model
// id are slugified to prevent ':' in paths from breaking the delimiter scheme.
// When either is empty a sha256 of the label+members is used as a stable fallback
// so two genuinely different hyperedges cannot collide.
func hyperedgeID(repo, sourceFile, modelID, label string, members []string) string {
	sf := slugLabel(sourceFile)
	mid := slugLabel(modelID)
	if sf == "" || mid == "" {
		h := sha256.New()
		h.Write([]byte(label))
		for _, m := range members {
			h.Write([]byte(m))
		}
		return fmt.Sprintf("he:%s:%x", repo, h.Sum(nil)[:8])
	}
	return fmt.Sprintf("he:%s:%s:%s", repo, sf, mid)
}

// stripFences finds the first '{' in body and returns the suffix starting there.
// A json.Decoder on this suffix stops at the end of the first valid JSON value,
// so trailing prose (including prose containing '}') is ignored.
func stripFences(body []byte) []byte {
	idx := bytes.IndexByte(body, '{')
	if idx == -1 {
		return body
	}
	return body[idx:]
}
