package analyze

import (
	"context"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSourceFileCachedPerFile verifies that each in-scope file is read from disk
// exactly once per Analyze call, even when the file contains multiple
// func/method declarations.
//
// RED (before fix): pkg/pkg.go has two functions (F and G), so sourceSlice is
// called twice for the same file, causing two os.ReadFile calls per file.
// GREEN (after fix): processPackage caches the bytes per absFilePath and
// sourceSliceBytes slices from the cache, so os.ReadFile is called once per file.
func TestSourceFileCachedPerFile(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "testdata", "go")

	// Instrument the package-level read hook.
	var readCount int64
	origReadFile := osReadFile
	osReadFile = func(name string) ([]byte, error) {
		atomic.AddInt64(&readCount, 1)
		return origReadFile(name)
	}
	defer func() { osReadFile = origReadFile }()

	a := NewGo("github.com/szymonrychu/")
	// pkg/pkg.go has two functions: F and G.
	// With the cache fix, the file must be read exactly once.
	_, err := a.Analyze(context.Background(), repoRoot, []string{"pkg/pkg.go"})
	require.NoError(t, err)

	require.EqualValues(t, 1, atomic.LoadInt64(&readCount),
		"pkg/pkg.go has 2 funcs but must be read from disk exactly once (file-level cache)")
}
