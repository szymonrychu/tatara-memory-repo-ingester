package analyze

import (
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFallbackAnalyzeGoPackageNoDuplicatePackageEntity verifies that
// fallbackAnalyzeGoPackage emits exactly one go:package entity per unique
// package path, even when multiple files share that package.
func TestFallbackAnalyzeGoPackageNoDuplicatePackageEntity(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "testdata", "go_broken")
	absRoot, err := filepath.Abs(repoRoot)
	require.NoError(t, err)

	absFile1 := filepath.Join(absRoot, "pkg", "broken.go")
	absFile2 := filepath.Join(absRoot, "pkg", "broken2.go")

	log := slog.Default()
	res := fallbackAnalyzeGoPackage(log, "example.com/broken", absRoot, []string{absFile1, absFile2})

	const pkgID = "go:package:example.com/broken/pkg"
	count := 0
	for _, e := range res.Entities {
		if e.ID == pkgID {
			count++
		}
	}
	require.Equal(t, 1, count, "go:package entity must appear exactly once; got %d", count)
}

// TestFallbackAnalyzeGoPackageScopeRedundancy demonstrates that the scope
// parameter in fallbackAnalyzeGoPackage was redundant: the caller
// (goAnalyzer.Analyze) pre-filters pkgFiles to only in-scope files before
// calling this function, so an additional scope check inside the function is
// dead code. After removing the scope parameter, all entries in pkgFiles are
// processed unconditionally.
//
// RED (before fix): calling fallbackAnalyzeGoPackage with a non-empty pkgFiles
// but an empty scope map would skip all files and return an empty result,
// demonstrating the dead branch could silently swallow valid files if the
// caller contract were ever violated.
// GREEN (after fix): the function accepts no scope; all pkgFiles are processed.
func TestFallbackAnalyzeGoPackageScopeRedundancy(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "testdata", "go_broken")
	absRoot, err := filepath.Abs(repoRoot)
	require.NoError(t, err)

	absFile := filepath.Join(absRoot, "pkg", "broken.go")

	log := slog.Default()

	// After removing scope from the signature, this call processes absFile
	// unconditionally.  The result must contain at least the H and G entities.
	res := fallbackAnalyzeGoPackage(log, "example.com/broken", absRoot, []string{absFile})

	entityIDs := map[string]bool{}
	for _, e := range res.Entities {
		entityIDs[e.ID] = true
	}

	require.True(t, entityIDs["go:func:example.com/broken/pkg.H"],
		"expected H entity; got: %v", entityIDs)
	require.True(t, entityIDs["go:func:example.com/broken/pkg.G"],
		"expected G entity; got: %v", entityIDs)
}
