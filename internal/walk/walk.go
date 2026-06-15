// Package walk lists the repository files to ingest.
package walk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Change is one touched file classified by git status.
type Change struct {
	Path       string // new path (for renames, the destination)
	OldPath    string // populated only for renames
	Status     rune   // 'A' added, 'M' modified, 'D' deleted, 'R' renamed
	ContentSHA string // sha256 of working-tree content for A/M; empty for D
}

// Changes is the classified diff result.
type Changes struct {
	Files   []Change // every touched file
	FullSet bool     // true when produced by ls-files (first/full/fallback)
}

// Diff classifies the files to ingest. With full or empty since, it lists all
// tracked files as additions (FullSet). Otherwise it runs
// `git diff --name-status <since>..HEAD`; if that command fails (since not an
// ancestor: force-push, rebase, GC'd commit) it logs WARN and falls back to the
// full ls-files path so a job never hard-fails on history rewrite.
func Diff(repoRoot, since string, full bool) (Changes, error) {
	if full || since == "" {
		return fullSet(repoRoot)
	}
	out, err := exec.Command("git", "-C", repoRoot, "diff", "-M", "--name-status", since+"..HEAD").Output() //nolint:gosec
	if err != nil {
		slog.Warn("git diff failed; falling back to full ls-files",
			"since", since, "error", err)
		return fullSet(repoRoot)
	}
	return parseDiff(repoRoot, string(out))
}

// fullSet lists all tracked files as additions.
func fullSet(repoRoot string) (Changes, error) {
	out, err := exec.Command("git", "-C", repoRoot, "ls-files").Output() //nolint:gosec
	if err != nil {
		return Changes{}, fmt.Errorf("git -C %s ls-files: %w", repoRoot, err)
	}
	var files []Change
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		files = append(files, Change{Path: line, Status: 'A', ContentSHA: contentSHA(repoRoot, line)})
	}
	sortChanges(files)
	return Changes{Files: files, FullSet: true}, nil
}

// parseDiff turns `git diff --name-status` output into classified changes.
// Rename lines `R<score>\told\tnew` become a single Change with Status 'R',
// OldPath=old, Path=new. Copy lines `C<score>` are treated as additions of new.
func parseDiff(repoRoot, out string) (Changes, error) {
	var files []Change
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		code := fields[0]
		if code == "" {
			continue
		}
		switch code[0] {
		case 'A', 'M':
			if len(fields) < 2 {
				slog.Warn("parseDiff: short line, skipping", "line", line)
				continue
			}
			p := fields[1]
			files = append(files, Change{Path: p, Status: rune(code[0]), ContentSHA: contentSHA(repoRoot, p)})
		case 'D':
			if len(fields) < 2 {
				slog.Warn("parseDiff: short line, skipping", "line", line)
				continue
			}
			files = append(files, Change{Path: fields[1], Status: 'D'})
		case 'R':
			if len(fields) < 3 {
				slog.Warn("parseDiff: short line, skipping", "line", line)
				continue
			}
			files = append(files, Change{
				Path: fields[2], OldPath: fields[1], Status: 'R',
				ContentSHA: contentSHA(repoRoot, fields[2]),
			})
		case 'C':
			if len(fields) < 3 {
				slog.Warn("parseDiff: short line, skipping", "line", line)
				continue
			}
			p := fields[2]
			files = append(files, Change{Path: p, Status: 'A', ContentSHA: contentSHA(repoRoot, p)})
		case 'T':
			// Type-change: treat as modification so the new content is re-ingested.
			if len(fields) < 2 {
				slog.Warn("parseDiff: short line, skipping", "line", line)
				continue
			}
			p := fields[1]
			files = append(files, Change{Path: p, Status: 'M', ContentSHA: contentSHA(repoRoot, p)})
		default:
			slog.Warn("parseDiff: unrecognized status code, skipping", "code", code, "line", line)
		}
	}
	sortChanges(files)
	return Changes{Files: files, FullSet: false}, nil
}

// contentSHA returns the sha256 hex of the working-tree file; empty when unreadable.
// For symlinks, it hashes the link target text (matching git blob semantics) rather
// than following the link, preventing reads of files outside the repo root.
func contentSHA(repoRoot, rel string) string {
	full := filepath.Join(repoRoot, rel) //nolint:gosec
	fi, err := os.Lstat(full)
	if err != nil {
		return ""
	}
	var b []byte
	if fi.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(full)
		if rerr != nil {
			return ""
		}
		b = []byte(target)
	} else {
		b, err = os.ReadFile(full) //nolint:gosec
		if err != nil {
			return ""
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sortChanges orders by new path for deterministic output.
func sortChanges(files []Change) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}
