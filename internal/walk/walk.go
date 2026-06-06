// Package walk lists the repository files to ingest.
package walk

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Changed returns repo-relative paths to ingest. With full or an empty since, it
// lists all tracked files; otherwise it diffs since..HEAD.
func Changed(repoRoot, since string, full bool) ([]string, error) {
	var args []string
	if full || since == "" {
		args = []string{"-C", repoRoot, "ls-files"}
	} else {
		args = []string{"-C", repoRoot, "diff", "--name-only", since + "..HEAD"}
	}
	out, err := exec.Command("git", args...).Output() //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}
