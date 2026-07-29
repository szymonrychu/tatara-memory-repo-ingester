package push

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/contract"
)

// graphFixture builds a push with nFiles files, each owning perFile entities and
// perFile edges, so row counts are predictable.
func graphFixture(nFiles, perFile int) contract.GraphPush {
	p := contract.GraphPush{Repo: "r", Commit: "c", Extractor: contract.ExtractorAST, FileSHAs: map[string]string{}}
	for i := 0; i < nFiles; i++ {
		f := fmt.Sprintf("pkg/f%03d.go", i)
		p.Files = append(p.Files, f)
		p.FileSHAs[f] = fmt.Sprintf("sha%03d", i)
		for j := 0; j < perFile; j++ {
			p.Entities = append(p.Entities, contract.Entity{ID: fmt.Sprintf("%s#e%d", f, j), FilePath: f})
			p.Edges = append(p.Edges, contract.Edge{From: fmt.Sprintf("%s#e%d", f, j), To: "other", Relation: contract.RelCalls, SrcFile: f})
			p.Symbols = append(p.Symbols, contract.SymbolRow{Symbol: fmt.Sprintf("s%d", j), Role: contract.RoleProvides, SrcFile: f})
			p.Hyperedges = append(p.Hyperedges, contract.Hyperedge{ID: fmt.Sprintf("%s#h%d", f, j), SrcFile: f})
		}
	}
	return p
}

// assertPartition checks that batches together cover the original push exactly
// once, and that every batch is self-consistent (every row's owning file is in
// that batch's Files, which the server validates and 400s on).
func assertPartition(t *testing.T, p contract.GraphPush, batches []contract.GraphPush, maxRows, maxFiles int) {
	t.Helper()
	seenFiles := map[string]int{}
	seenEntities, seenEdges, seenSymbols, seenHyper := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	seenSHAs := map[string]int{}
	for bi, b := range batches {
		require.Equal(t, p.Repo, b.Repo)
		require.Equal(t, p.Commit, b.Commit)
		require.Equal(t, p.Extractor, b.Extractor)
		require.NotEmpty(t, b.Files, "batch %d must carry files (the server 400s on an empty files set)", bi)
		assert.LessOrEqual(t, len(b.Files), maxFiles, "batch %d exceeds the file cap", bi)
		files := map[string]struct{}{}
		for _, f := range b.Files {
			files[f] = struct{}{}
			seenFiles[f]++
		}
		for _, e := range b.Entities {
			seenEntities[e.ID]++
			if e.FilePath != "" {
				_, ok := files[e.FilePath]
				assert.True(t, ok, "batch %d entity %s file %q not in batch files", bi, e.ID, e.FilePath)
			}
		}
		for _, e := range b.Edges {
			seenEdges[e.From+"->"+e.To+":"+e.SrcFile]++
			_, ok := files[e.SrcFile]
			assert.True(t, ok, "batch %d edge src_file %q not in batch files", bi, e.SrcFile)
		}
		for _, s := range b.Symbols {
			seenSymbols[s.Symbol+"@"+s.SrcFile]++
			_, ok := files[s.SrcFile]
			assert.True(t, ok, "batch %d symbol src_file %q not in batch files", bi, s.SrcFile)
		}
		for _, h := range b.Hyperedges {
			seenHyper[h.ID]++
		}
		for path := range b.FileSHAs {
			seenSHAs[path]++
		}
		assert.LessOrEqual(t, batchRows(b), maxRows,
			"batch %d exceeds the row cap and is not a single oversized file", bi)
	}
	assert.Len(t, seenFiles, len(p.Files), "every file must appear exactly once across batches")
	for f, n := range seenFiles {
		assert.Equal(t, 1, n, "file %s appears in %d batches", f, n)
	}
	assert.Len(t, seenEntities, len(p.Entities))
	assert.Len(t, seenEdges, len(p.Edges))
	assert.Len(t, seenSymbols, len(p.Symbols))
	assert.Len(t, seenHyper, len(p.Hyperedges))
	assert.Len(t, seenSHAs, len(p.FileSHAs))
	for k, n := range seenEntities {
		assert.Equal(t, 1, n, "entity %s duplicated across batches", k)
	}
}

func TestSplitGraphPush(t *testing.T) {
	tests := []struct {
		name            string
		push            contract.GraphPush
		maxRows         int
		maxFiles        int
		wantBatches     int
		skipRowCapCheck bool
	}{
		{
			name:        "small graph stays a single request",
			push:        graphFixture(10, 2),
			maxRows:     MaxBatchRows,
			maxFiles:    MaxBatchFiles,
			wantBatches: 1,
		},
		{
			name:        "row cap splits the graph",
			push:        graphFixture(100, 4), // 100 files x 16 rows = 1600 rows
			maxRows:     400,
			maxFiles:    1000,
			wantBatches: 4,
		},
		{
			name:        "file cap splits the graph",
			push:        graphFixture(100, 1),
			maxRows:     1_000_000,
			maxFiles:    25,
			wantBatches: 4,
		},
		{
			name:        "no files means nothing to partition on",
			push:        contract.GraphPush{Repo: "r", Entities: []contract.Entity{{ID: "x"}}},
			maxRows:     1,
			maxFiles:    1,
			wantBatches: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			batches := splitGraphPush(tc.push, tc.maxRows, tc.maxFiles)
			require.Len(t, batches, tc.wantBatches)
			if len(tc.push.Files) > 0 {
				assertPartition(t, tc.push, batches, tc.maxRows, tc.maxFiles)
			}
		})
	}
}

// TestSplitGraphPushKeepsFileAtomic asserts a single file whose rows exceed the
// cap is still shipped whole: splitting one file across two requests would let
// the second batch's reconcile purge what the first batch just inserted.
func TestSplitGraphPushKeepsFileAtomic(t *testing.T) {
	p := graphFixture(3, 50) // 200 rows per file
	batches := splitGraphPush(p, 10, 100)
	require.Len(t, batches, 3)
	for _, b := range batches {
		require.Len(t, b.Files, 1)
		require.Equal(t, 200, batchRows(b))
	}
}

// TestSplitGraphPushUnfiledRowsGoToFirstBatch pins where rows with no owning
// file land. The server explicitly allows an entity with an empty file_path
// (repo/package-scoped entities such as go_package), so those rows must be sent
// exactly once rather than dropped or duplicated per batch.
func TestSplitGraphPushUnfiledRowsGoToFirstBatch(t *testing.T) {
	p := graphFixture(10, 2)
	p.Entities = append(p.Entities, contract.Entity{ID: "go_package:pkg", FilePath: ""})
	p.FileSHAs["not/in/files.go"] = "orphan"
	batches := splitGraphPush(p, 8, 100)
	require.Greater(t, len(batches), 1)

	var found int
	for bi, b := range batches {
		for _, e := range b.Entities {
			if e.ID == "go_package:pkg" {
				found++
				assert.Equal(t, 0, bi, "unfiled entity must ride in the first batch")
			}
		}
		if _, ok := b.FileSHAs["not/in/files.go"]; ok {
			assert.Equal(t, 0, bi, "unfiled file_sha must ride in the first batch")
		}
	}
	require.Equal(t, 1, found, "unfiled entity must be sent exactly once")
}

// TestSplitGraphPushPreservesFileOrder keeps batches deterministic so a retry of
// the same window replays the same batch boundaries.
func TestSplitGraphPushPreservesFileOrder(t *testing.T) {
	p := graphFixture(20, 2)
	batches := splitGraphPush(p, 16, 100)
	var got []string
	for _, b := range batches {
		got = append(got, b.Files...)
	}
	require.Equal(t, p.Files, got)
}
