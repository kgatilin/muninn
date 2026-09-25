package fsconn

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/stream"
)

func TestChunkerForChoosesPerFile(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"a/main.go": "package main\n", "a/notes.md": "# Notes\n", "b.txt": "plain\n"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := Run(stream.NewWriter(&buf), Options{Root: root, Globs: []string{"**/*"}, ChunkerFor: []string{"**/*.go=go", "**/*.txt=none"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for r := stream.NewReader(&buf); ; {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rec.Node != nil && rec.Node.Kind == "document" {
			got[rec.Node.Attrs["path"]] = rec.Node.Chunker
		}
	}
	want := map[string]string{"a/main.go": "go", "a/notes.md": "", "b.txt": "none"}
	for path, chunker := range want {
		if c, ok := got[path]; !ok || c != chunker {
			t.Errorf("%s: chunker %q (emitted %v), want %q", path, c, ok, chunker)
		}
	}
}

func TestChunkerNamesAreCheckedBeforeTheWalk(t *testing.T) {
	for _, o := range []Options{{ChunkerFor: []string{"**/*.go=golang"}}, {ChunkerFor: []string{"**/*.go"}}, {Chunker: "golang"}} {
		o.Root = t.TempDir()
		var buf bytes.Buffer
		err := Run(stream.NewWriter(&buf), o)
		if err == nil || buf.Len() > 0 {
			t.Errorf("%+v: err %v, %d bytes written", o, err, buf.Len())
		} else if o.Chunker != "" && !strings.Contains(err.Error(), "have go, none, text") {
			t.Errorf("the error does not list the chunkers: %v", err)
		}
	}
}

func TestFollowSymlinksReadsThroughTheLink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"own.md": "# Own\n", "sub/in.md": "# In\n"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "deep", "far.md"), []byte("# Far\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"repo":     outside,                       // a sub-repo elsewhere
		"again":    outside,                       // a second link to it
		"inside":   filepath.Join(root, "sub"),    // a link into the tree
		"one.md":   filepath.Join(root, "own.md"), // a linked file
		"repo2/up": root,                          // a link back up
	} {
		path := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	paths := func(follow bool) []string {
		var buf bytes.Buffer
		if err := Run(stream.NewWriter(&buf), Options{Root: root, FollowSymlinks: follow}); err != nil {
			t.Fatal(err)
		}
		var out []string
		for r := stream.NewReader(&buf); ; {
			rec, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if rec.Node != nil && rec.Node.Kind == "document" {
				out = append(out, rec.Node.Attrs["path"])
			}
		}
		sort.Strings(out)
		return out
	}
	if got, want := paths(false), []string{"own.md", "sub/in.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("without following: %v, want %v", got, want)
	}
	// "again" or "repo", whichever the walk reaches first, is the one read.
	if got, want := paths(true), []string{"again/deep/far.md", "one.md", "own.md", "sub/in.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("following: %v, want %v", got, want)
	}
}
