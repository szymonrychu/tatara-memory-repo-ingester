package analyze_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
)

// This file is the forcing function for the FailedFiles contract documented on
// analyze.Result: a diff-set file that an analyzer skips without emitting rows
// MUST be reported in FailedFiles, or the caller keeps it in both reconcile
// scopes and tatara-memory purges its last-good rows with no replacement.
//
// The class has landed three times (#19, #37, #40), each time found by reading
// rather than by CI, and each fix was per-site. A per-site test cannot fail when
// analyzer number seven arrives with a new skip path; a TOTAL table over the
// registry can. TestFailedFilesContract_CoversEveryAnalyzer is that assertion and
// it runs unconditionally, so a new analyzer with no row here fails CI even
// though every existing row still passes.

// failedFilesCase drives one analyzer with a fixture whose diff-set file cannot
// be processed, and asserts that file is reported.
type failedFilesCase struct {
	// analyzer is the registry name this row covers; it must match Analyzer.Name().
	analyzer string
	// skipReason, when non-empty, records a MEASURED reason this analyzer cannot be
	// driven black-box. The row still counts for totality coverage.
	skipReason string
	// build writes the fixture into dir and returns the analyzer, the diff-set file
	// list, and the paths required to appear in FailedFiles.
	build func(t *testing.T, dir string) (a analyze.Analyzer, files, wantFailed []string)
}

// writeAt writes content to dir/name, creating parent directories.
func writeAt(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

// danglingAt creates a symlink at dir/name pointing at a path that does not exist.
// os.ReadFile then fails with ENOENT regardless of uid, unlike chmod 000 which is
// a no-op as root and would force a skip on whichever CI runner runs as root.
func danglingAt(t *testing.T, dir, name string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(dir, "no-such-target"), full))
}

func failedFilesCases() []failedFilesCase {
	return []failedFilesCase{
		{
			analyzer: "go",
			// Measured, not assumed: go/packages omits an unreadable file from
			// pkg.GoFiles entirely (it surfaces only as a pkg.Errors entry whose
			// message is an unstructured string). goAnalyzer.Analyze iterates
			// pkg.GoFiles, so the fallback read-failure branch in
			// fallbackAnalyzeGoPackage is unreachable from any black-box fixture:
			// the fallback only ever receives files go/packages already read.
			//
			// The same omission means a diff-set .go file that lands in no loaded
			// package produces nothing and is not recorded. That is a real instance
			// of this class, but the fix is not the one-liner the other rows take -
			// every *_test.go file is likewise absent from GoFiles (Tests is false),
			// as is every build-tag-excluded file, and recording those would pin the
			// soft-failure counter permanently above zero. It needs its own change;
			// see MEMORY.md.
			skipReason: "go/packages omits unreadable files from pkg.GoFiles, so the fallback read-failure branch is unreachable black-box",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				return analyze.NewGo(""), nil, nil
			},
		},
		{
			analyzer: "python",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				danglingAt(t, dir, "pkg/broken.py")
				return analyze.NewPython(), []string{"pkg/broken.py"}, []string{"pkg/broken.py"}
			},
		},
		{
			analyzer: "javascript",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				danglingAt(t, dir, "src/broken.js")
				return analyze.NewJavaScript(), []string{"src/broken.js"}, []string{"src/broken.js"}
			},
		},
		{
			analyzer: "terraform",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				// An unterminated block is a deterministic HCL parse error - no
				// filesystem trick needed.
				writeAt(t, dir, "main.tf", "resource \"aws_s3_bucket\" \"b\" {\n")
				return analyze.NewTerraform(), []string{"main.tf"}, []string{"main.tf"}
			},
		},
		{
			analyzer: "helm",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				// A nameless Chart.yaml skips the whole chart, so values.yaml and
				// the template produce nothing either. All three are diff files.
				writeAt(t, dir, "chart/Chart.yaml", "apiVersion: v2\nversion: 0.1.0\n")
				writeAt(t, dir, "chart/values.yaml", "key: val\n")
				writeAt(t, dir, "chart/templates/cm.yaml", "data: {{ .Values.key }}\n")
				files := []string{"chart/Chart.yaml", "chart/values.yaml", "chart/templates/cm.yaml"}
				return analyze.NewHelm(dir), files, files
			},
		},
		{
			analyzer: "docs",
			build: func(t *testing.T, dir string) (analyze.Analyzer, []string, []string) {
				danglingAt(t, dir, "docs/broken.md")
				return analyze.NewDocs(), []string{"docs/broken.md"}, []string{"docs/broken.md"}
			},
		},
	}
}

// TestFailedFilesContract_CoversEveryAnalyzer is the forcing function. It asserts
// the table names EXACTLY the registry, so adding an analyzer without thinking
// about its skip paths fails here even if every other row is green.
func TestFailedFilesContract_CoversEveryAnalyzer(t *testing.T) {
	var registered []string
	for _, a := range analyze.Default("", t.TempDir()).Analyzers() {
		registered = append(registered, a.Name())
	}
	var covered []string
	for _, c := range failedFilesCases() {
		covered = append(covered, c.analyzer)
	}
	sort.Strings(registered)
	sort.Strings(covered)
	require.Equal(t, registered, covered,
		"every registered analyzer needs a FailedFiles contract row: a diff-set file it "+
			"skips without emitting rows must be reported, or the caller purges that file's "+
			"last-good rows with no replacement")
}

// TestFailedFilesContract_UnprocessableFileIsRecorded drives each row.
func TestFailedFilesContract_UnprocessableFileIsRecorded(t *testing.T) {
	for _, c := range failedFilesCases() {
		t.Run(c.analyzer, func(t *testing.T) {
			if c.skipReason != "" {
				t.Skip(c.skipReason)
			}
			dir := t.TempDir()
			a, files, wantFailed := c.build(t, dir)
			require.Equal(t, c.analyzer, a.Name(), "row name must match the analyzer it builds")

			res, err := a.Analyze(context.Background(), dir, files)
			require.NoError(t, err, "an unprocessable single file is a soft failure, not a batch error")

			for _, f := range wantFailed {
				require.Contains(t, res.FailedFiles, f,
					"%s: %q is in the diff set and produced nothing, so it must be in FailedFiles",
					c.analyzer, f)
			}
		})
	}
}
