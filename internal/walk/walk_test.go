package walk_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/walk"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run())
	}
	return dir
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run())
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	return string(out[:len(out)-1])
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// byPath indexes a Changes result by new path for assertions.
func byPath(c walk.Changes) map[string]walk.Change {
	m := map[string]walk.Change{}
	for _, ch := range c.Files {
		m[ch.Path] = ch
	}
	return m
}

func TestFullWalkListsTrackedFiles(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a")
	write(t, dir, "sub/b.py", "x = 1")
	commit(t, dir, "init")

	got, err := walk.Diff(dir, "", false)
	require.NoError(t, err)
	require.True(t, got.FullSet)
	m := byPath(got)
	require.Len(t, m, 2)
	require.Equal(t, 'A', m["a.go"].Status)
	require.Equal(t, 'A', m["sub/b.py"].Status)
	require.Equal(t, sha("package a"), m["a.go"].ContentSHA)
}

func TestFullFlagOverridesSince(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a")
	base := commit(t, dir, "init")
	write(t, dir, "c.go", "package c")
	commit(t, dir, "add c")

	got, err := walk.Diff(dir, base, true)
	require.NoError(t, err)
	require.True(t, got.FullSet)
	m := byPath(got)
	require.Len(t, m, 2)
	require.Contains(t, m, "a.go")
	require.Contains(t, m, "c.go")
}
