package sessconn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// worktreeDirs are the directories worktrees are kept in, inside the repo they
// belong to: <repo>/.worktrees/<name> is what `wt` makes, and
// <repo>/.claude/worktrees/<name> is Claude Code's own.
var worktreeDirs = []string{"/.worktrees/", "/.claude/worktrees/"}

var (
	rootsMu sync.Mutex
	roots   = map[string]string{}
)

// ProjectRoot is the main repository a session's directory belongs to, so a
// session run in a worktree or a subdirectory is filed under its repo.
//
// The path is asked first, because it still answers after the worktree is
// gone: anything under <repo>/.worktrees/ belongs to <repo>. Then the disk:
// the nearest .git upward is the repo when it is a directory, and when it is a
// file — a worktree kept anywhere else — its gitdir names the main repo. With
// neither, the directory stands for itself.
func ProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	rootsMu.Lock()
	defer rootsMu.Unlock()
	if root, ok := roots[cwd]; ok {
		return root
	}
	root := projectRoot(filepath.Clean(cwd))
	roots[cwd] = root
	return root
}

func projectRoot(cwd string) string {
	for _, dir := range worktreeDirs {
		if i := strings.Index(cwd+"/", dir); i > 0 {
			return cwd[:i]
		}
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		git := filepath.Join(dir, ".git")
		if info, err := os.Stat(git); err == nil {
			if info.IsDir() {
				return dir
			}
			if main := mainRepo(git); main != "" {
				return main
			}
			return dir
		}
		if dir == filepath.Dir(dir) {
			return cwd
		}
	}
}

// mainRepo reads a worktree's .git file, "gitdir: <repo>/.git/worktrees/<name>",
// and returns <repo>. A submodule's .git file points elsewhere and gives "".
func mainRepo(gitFile string) string {
	raw, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if i := strings.Index(gitdir, "/.git/worktrees/"); i > 0 {
		return gitdir[:i]
	}
	return ""
}

// Match is the --project scope.
type Match struct {
	paths []string
	names []string
}

// NewMatch reads --project values. One with a path separator, or starting
// with ~ or ., is a path; anything else is a name.
func NewMatch(projects []string) (Match, error) {
	var m Match
	for _, p := range projects {
		if p == "" {
			continue
		}
		if !strings.ContainsRune(p, filepath.Separator) && !strings.HasPrefix(p, "~") && !strings.HasPrefix(p, ".") {
			m.names = append(m.names, p)
			continue
		}
		abs, err := filepath.Abs(expandHome(p))
		if err != nil {
			return m, err
		}
		m.paths = append(m.paths, abs)
	}
	return m, nil
}

// Session reports whether a session belongs to one of the projects. Both its
// directory and the main repo derived from it are asked, so a worktree counts
// wherever it is kept: one inside the repo matches by its directory, one kept
// beside the repo matches by the repo its .git file names.
//
// A path matches a directory equal to it or under it. A name matches a
// directory that has the name as one of its path segments.
func (m Match) Session(cwd, project string) bool {
	if len(m.paths) == 0 && len(m.names) == 0 {
		return true
	}
	for _, dir := range []string{cwd, project} {
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		for _, p := range m.paths {
			if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) {
				return true
			}
		}
		for _, segment := range strings.Split(dir, string(filepath.Separator)) {
			for _, name := range m.names {
				if segment == name {
					return true
				}
			}
		}
	}
	return false
}
