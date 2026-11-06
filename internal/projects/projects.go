// Package projects finds the folders a session can be started in from the
// phone: the git repositories under one root folder, at any level, since a
// root often groups them by client.
package projects

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxDepth is how many folders below the root a project can sit.
const MaxDepth = 4

// worktrees is where Claude Code keeps a project's worktrees.
const worktrees = "/.claude/worktrees/"

// Find returns the repositories under root, sorted. It does not follow
// links, which can point outside root, and skips hidden folders and
// dependency folders. A repository inside a repository counts. A folder it
// cannot read is skipped.
func Find(ctx context.Context, root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root || !d.IsDir() {
			return nil
		}
		if name := d.Name(); strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
			return fs.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			found = append(found, path)
		}
		if strings.Count(Shown(root, path), string(filepath.Separator)) >= MaxDepth-1 {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// Home is the project dir belongs to: a worktree counts as its project.
func Home(dir string) string {
	if i := strings.Index(dir, worktrees); i > 0 {
		return dir[:i]
	}
	return dir
}

// Shown is dir as the phone shows it: relative to root.
func Shown(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return rel
}

// Match returns the dirs whose path under root has every word of query,
// ignoring case: those named exactly query when there are any, else the
// rest with the best first (a folder name that starts with the first word,
// then one that has it, then a path that does).
func Match(dirs []string, root, query string) []string {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil
	}
	whole := strings.Join(words, " ")
	type hit struct {
		dir, shown string
		rank       int
	}
	var hits []hit
	for _, dir := range dirs {
		shown := strings.ToLower(Shown(root, dir))
		if !hasAll(shown, words) {
			continue
		}
		base := strings.ToLower(filepath.Base(dir))
		rank := 3
		switch {
		case base == whole:
			rank = 0
		case strings.HasPrefix(base, words[0]):
			rank = 1
		case strings.Contains(base, words[0]):
			rank = 2
		}
		hits = append(hits, hit{dir, shown, rank})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].shown < hits[j].shown
	})
	var out []string
	for _, h := range hits {
		if hits[0].rank == 0 && h.rank != 0 {
			break
		}
		out = append(out, h.dir)
	}
	return out
}

func hasAll(s string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

// Under reports whether dir is an existing folder below root, links
// resolved.
func Under(root, dir string) bool {
	_, ok := Resolve(root, dir)
	return ok
}

// Resolve returns dir with its links resolved, and whether that is an
// existing folder below root. A session starts in the path it returns, so
// that what was checked is what opens.
func Resolve(root, dir string) (string, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	if st, err := os.Stat(realDir); err != nil || !st.IsDir() {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return realDir, true
}
