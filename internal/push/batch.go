package push

import (
	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// Batch caps for /code-graph:bulk. tatara-memory reconciles one push in ONE
// Postgres transaction, row at a time (tatara-memory internal/codegraph/pgstore.go
// Reconcile), and its own handler notes a whole-repo push legitimately runs
// 50-194s - long enough to monopolise a connection of the shared pool and starve
// /memories:bulk (tatara-memory#82, #79/#80/#87). Bounding the client payload is
// what bounds that transaction.
//
// Sizing: a measured full ingest of this repo produced 939 rows / 291 KB across
// 137 files, i.e. ~310 bytes per row. MaxBatchRows=2000 is therefore ~620 KB per
// request (the server's own body cap is 32 MiB, so bytes are not the binding
// constraint - transaction duration is) and ~2000 statements per transaction,
// which lands in the sub-second range instead of minutes. MaxBatchFiles=250
// bounds the other half of the work: reconcile issues 4-5 DELETEs per file
// before any insert, so a file-heavy push (deletions, renames) is capped even
// when it carries almost no rows. Both caps leave the common incremental push -
// and a small repo's full ingest - as a single request, so batching does not
// multiply per-transaction overhead on small repos.
const (
	MaxBatchRows  = 2000
	MaxBatchFiles = 250
)

// batchRows counts the rows a push carries.
func batchRows(p contract.GraphPush) int {
	return len(p.Entities) + len(p.Edges) + len(p.Symbols) + len(p.Hyperedges)
}

// fileRows holds the rows owned by one file.
type fileRows struct {
	entities   []contract.Entity
	edges      []contract.Edge
	symbols    []contract.SymbolRow
	hyperedges []contract.Hyperedge
	shas       map[string]string
}

func (f *fileRows) count() int {
	return len(f.entities) + len(f.edges) + len(f.symbols) + len(f.hyperedges)
}

// splitGraphPush partitions p into batches of at most maxRows rows and maxFiles
// files. Each batch is a complete, independently valid /code-graph:bulk request:
// it carries its own files subset plus exactly the rows those files own, so the
// server's per-file reconcile (delete-then-insert, scoped by src_file and
// extractor) stays atomic per file. A file is never split across batches - the
// second batch's reconcile would purge what the first just inserted - so one
// file whose rows exceed maxRows ships alone in an oversized batch.
//
// Rows with no owning file in p.Files ride in the first batch: an entity with an
// empty file_path is legitimate (repo/package-scoped entities such as
// go_package, which the server explicitly allows), and anything else that does
// not match p.Files is a client bug the server must still reject loudly rather
// than have it silently dropped here.
//
// A push with no Files has nothing to partition on and is returned unchanged.
func splitGraphPush(p contract.GraphPush, maxRows, maxFiles int) []contract.GraphPush {
	if len(p.Files) == 0 || (batchRows(p) <= maxRows && len(p.Files) <= maxFiles) {
		return []contract.GraphPush{p}
	}

	owned := make(map[string]*fileRows, len(p.Files))
	for _, f := range p.Files {
		owned[f] = &fileRows{}
	}
	unfiled := &fileRows{}
	at := func(file string) *fileRows {
		if fr, ok := owned[file]; ok {
			return fr
		}
		return unfiled
	}
	for _, e := range p.Entities {
		fr := unfiled
		if e.FilePath != "" {
			fr = at(e.FilePath)
		}
		fr.entities = append(fr.entities, e)
	}
	for _, e := range p.Edges {
		fr := at(e.SrcFile)
		fr.edges = append(fr.edges, e)
	}
	for _, s := range p.Symbols {
		fr := at(s.SrcFile)
		fr.symbols = append(fr.symbols, s)
	}
	for _, h := range p.Hyperedges {
		fr := at(h.SrcFile)
		fr.hyperedges = append(fr.hyperedges, h)
	}
	for path, sha := range p.FileSHAs {
		fr := at(path)
		if fr.shas == nil {
			fr.shas = map[string]string{}
		}
		fr.shas[path] = sha
	}

	var batches []contract.GraphPush
	cur := newBatch(p, unfiled)
	curRows := unfiled.count()
	for _, f := range p.Files {
		fr := owned[f]
		if len(cur.Files) > 0 && (curRows+fr.count() > maxRows || len(cur.Files) >= maxFiles) {
			batches = append(batches, cur)
			cur = newBatch(p, nil)
			curRows = 0
		}
		cur.Files = append(cur.Files, f)
		cur.Entities = append(cur.Entities, fr.entities...)
		cur.Edges = append(cur.Edges, fr.edges...)
		cur.Symbols = append(cur.Symbols, fr.symbols...)
		cur.Hyperedges = append(cur.Hyperedges, fr.hyperedges...)
		for path, sha := range fr.shas {
			if cur.FileSHAs == nil {
				cur.FileSHAs = map[string]string{}
			}
			cur.FileSHAs[path] = sha
		}
		curRows += fr.count()
	}
	return append(batches, cur)
}

// newBatch starts a batch carrying p's scope fields plus, when seed is set, the
// rows that belong to no file.
func newBatch(p contract.GraphPush, seed *fileRows) contract.GraphPush {
	b := contract.GraphPush{Repo: p.Repo, Commit: p.Commit, Extractor: p.Extractor}
	if seed == nil {
		return b
	}
	b.Entities = append(b.Entities, seed.entities...)
	b.Edges = append(b.Edges, seed.edges...)
	b.Symbols = append(b.Symbols, seed.symbols...)
	b.Hyperedges = append(b.Hyperedges, seed.hyperedges...)
	for path, sha := range seed.shas {
		if b.FileSHAs == nil {
			b.FileSHAs = map[string]string{}
		}
		b.FileSHAs[path] = sha
	}
	return b
}
