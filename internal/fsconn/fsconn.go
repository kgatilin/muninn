// Package fsconn is the file-system connector: a directory walk written to the
// graph stream. It is a full scan that ends in a sweep and keeps no state —
// unchanged hashes are what make the next run cheap.
package fsconn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/chunk"
	"github.com/kgatilin/muninn/internal/glob"
	"github.com/kgatilin/muninn/stream"
)

type Options struct {
	Root     string
	Globs    []string
	Excludes []string
	// Chunker is stated on every document; empty leaves it to the bank.
	Chunker string
	// ChunkerFor chooses per file: `<glob>=<chunker>`, the first glob that
	// matches the path under the root wins, and a file none matches gets Chunker.
	ChunkerFor []string
	// FollowSymlinks walks into linked directories and reads linked files, under
	// the path through the link. A link into the tree, or to a directory already
	// walked, is left out, so nothing is read twice and a cycle ends.
	FollowSymlinks bool
}

type chunkerRule struct {
	glob    *regexp.Regexp
	chunker string
}

// maxFile skips what is not a document anyone wrote by hand.
const maxFile = 4 << 20

var linkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func Run(w *stream.Writer, o Options) error {
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("root %s is not a directory", root)
	}
	if len(o.Globs) == 0 {
		o.Globs = []string{"**/*.md"}
	}
	include, err := glob.Compile(o.Globs)
	if err != nil {
		return err
	}
	exclude, err := glob.Compile(o.Excludes)
	if err != nil {
		return err
	}
	rules, err := chunkerRules(o)
	if err != nil {
		return err
	}

	added := gitAdded(root)

	dirs := map[string]bool{}
	var ensureDir func(dir string) error
	ensureDir = func(dir string) error {
		if dirs[dir] {
			return nil
		}
		dirs[dir] = true
		rel, _ := filepath.Rel(root, dir)
		if err := w.Node(stream.Node{ID: dir, Kind: "directory", Attrs: map[string]string{"path": filepath.ToSlash(rel)}}); err != nil {
			return err
		}
		if dir == root {
			return nil
		}
		parent := filepath.Dir(dir)
		if err := ensureDir(parent); err != nil {
			return err
		}
		return w.Edge(stream.Edge{From: parent, To: dir, Kind: "contains"})
	}

	// walked holds the real directories a walk has started from: the root and
	// every linked directory followed.
	var walked []string
	if real, err := filepath.EvalSymlinks(root); err == nil {
		walked = append(walked, real)
	}
	var visit fs.WalkDirFunc
	visit = func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			name := filepath.Base(path)
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || glob.Match(exclude, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		var info fs.FileInfo
		switch {
		case d.Type().IsRegular():
			if !glob.Match(include, rel) || glob.Match(exclude, rel) {
				return nil
			}
			if info, err = d.Info(); err != nil {
				return err
			}
		case d.Type()&fs.ModeSymlink != 0 && o.FollowSymlinks:
			if info, err = os.Stat(path); err != nil {
				fmt.Fprintf(os.Stderr, "skipped %s: %v\n", rel, err)
				return nil
			}
			if info.IsDir() {
				return follow(path, rel, &walked, added, visit)
			}
			if !info.Mode().IsRegular() || !glob.Match(include, rel) || glob.Match(exclude, rel) {
				return nil
			}
		default:
			return nil
		}
		if info.Size() > maxFile {
			fmt.Fprintf(os.Stderr, "skipped %s: %d bytes\n", rel, info.Size())
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(raw) {
			fmt.Fprintf(os.Stderr, "skipped %s: not UTF-8\n", rel)
			return nil
		}
		text := string(raw)
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		at := info.ModTime()
		if t, ok := added[rel]; ok {
			at = t
		}
		attrs := map[string]string{"path": rel}
		if t := title(text); t != "" {
			attrs["title"] = t
		}
		chunker := o.Chunker
		for _, r := range rules {
			if r.glob.MatchString(rel) {
				chunker = r.chunker
				break
			}
		}
		if err := w.Node(stream.Node{ID: path, Kind: "document", Text: text, Hash: hex.EncodeToString(sum[:16]), Attrs: attrs, Chunker: chunker, At: &at}); err != nil {
			return err
		}
		if err := w.Edge(stream.Edge{From: filepath.Dir(path), To: path, Kind: "contains"}); err != nil {
			return err
		}
		for _, target := range links(text, filepath.Dir(path), root) {
			if err := w.Edge(stream.Edge{From: path, To: target, Kind: "links"}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := filepath.WalkDir(root, visit); err != nil {
		return err
	}
	if err := w.Sweep(); err != nil {
		return err
	}
	return w.Flush()
}

// follow walks the directory a link at path names as if it stood at path, and
// takes git's times for it from the checkout it is in. A target that holds or
// sits in a directory already walked is left out: the root's own links into the
// tree, a second link to one place, and a link back up all end here.
func follow(path, rel string, walked *[]string, added map[string]time.Time, visit fs.WalkDirFunc) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	for _, w := range *walked {
		if within(real, w) || within(w, real) {
			return nil
		}
	}
	*walked = append(*walked, real)
	for p, t := range gitAdded(real) {
		added[rel+"/"+p] = t
	}
	return filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		under, _ := filepath.Rel(real, p)
		return visit(filepath.Join(path, under), d, err)
	})
}

// within says path is dir or under it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// chunkerRules parses the per-file rules and checks every chunker named, here
// and in Chunker, before anything is written: the engine would fail the run on
// an unknown one, after the walk.
func chunkerRules(o Options) ([]chunkerRule, error) {
	known := func(name string) error {
		if _, ok := chunk.Get(name); !ok {
			return fmt.Errorf("unknown chunker %q (have %s)", name, strings.Join(chunk.Names(), ", "))
		}
		return nil
	}
	if o.Chunker != "" {
		if err := known(o.Chunker); err != nil {
			return nil, err
		}
	}
	var rules []chunkerRule
	for _, spec := range o.ChunkerFor {
		i := strings.LastIndexByte(spec, '=')
		if i <= 0 || i == len(spec)-1 {
			return nil, fmt.Errorf("chunker rule %q is not <glob>=<chunker>", spec)
		}
		if err := known(spec[i+1:]); err != nil {
			return nil, err
		}
		res, err := glob.Compile([]string{spec[:i]})
		if err != nil {
			return nil, err
		}
		rules = append(rules, chunkerRule{glob: res[0], chunker: spec[i+1:]})
	}
	return rules, nil
}

// links resolves a document's relative links to files under the root. An edge
// to a file no glob matched is harmless: it names a node that never arrives.
func links(text, dir, root string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(text, -1) {
		target, _, _ := strings.Cut(m[1], "#")
		if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if decoded, err := url.PathUnescape(target); err == nil {
			target = decoded
		}
		abs := filepath.Clean(filepath.Join(dir, filepath.FromSlash(target)))
		if seen[abs] || !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			continue
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// title is the front matter's, or the first top-level heading.
func title(text string) string {
	inFront := strings.HasPrefix(text, "---\n")
	for i, line := range strings.SplitN(text, "\n", 60) {
		line = strings.TrimRight(line, "\r")
		switch {
		case inFront && i > 0 && line == "---":
			inFront = false
		case inFront:
			if v, ok := strings.CutPrefix(line, "title:"); ok {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		default:
			if v, ok := strings.CutPrefix(line, "# "); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// gitAdded is when git says each file under root was added, by its path under
// root. A checkout gives every file the time of the clone, and the time a
// document was written is what says which of two holds now. Outside a work
// tree, or without git, it is empty and the file's own time stands. A file
// that was moved is as old as the move.
func gitAdded(root string) map[string]time.Time {
	out := map[string]time.Time{}
	raw, err := exec.Command("git", "-C", root, "-c", "core.quotepath=off", "log", "--no-renames", "--diff-filter=A", "--name-only", "--relative", "--format=%x00%ct").Output()
	if err != nil {
		return out
	}
	var at time.Time
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "":
		case line[0] == 0:
			sec, _ := strconv.ParseInt(line[1:], 10, 64)
			at = time.Unix(sec, 0)
		default:
			// The log runs from the latest commit back: the first time a path is
			// seen is the last time it was added.
			if _, seen := out[line]; !seen {
				out[line] = at
			}
		}
	}
	return out
}
