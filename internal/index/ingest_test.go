package index_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/stream"
)

// A connector here is `cat <file>`: the engine's only input is a process's
// stdout, so a file of records is a connector.
func newBank(t *testing.T, streamFile string) *bank.Bank {
	t.Helper()
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, err := bank.New("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Set("embedder", "noop"); err != nil {
		t.Fatal(err)
	}
	b.Connectors = []bank.Connector{{Name: "src", Command: []string{"cat", streamFile}}}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeStream(t *testing.T, path string, write func(w *stream.Writer)) {
	t.Helper()
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	write(w)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func index1(t *testing.T, b *bank.Bank) *index.State {
	t.Helper()
	if err := index.Run(context.Background(), b, index.Options{}); err != nil {
		t.Fatal(err)
	}
	s, err := index.LoadState(b.IndexDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIngestSweepAndSearch(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	b := newBank(t, file)

	writeStream(t, file, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "/n/ravens.md", Kind: "document", Text: "# Ravens\n\nHuginn and Muninn fly over the world.", Attrs: map[string]string{"topic": "myth"}})
		w.Node(stream.Node{ID: "/n/bread.md", Kind: "document", Text: "# Bread\n\nSourdough needs a starter and patience.", Attrs: map[string]string{"topic": "food"}})
		w.Node(stream.Node{ID: "/n", Kind: "directory"})
		w.Edge(stream.Edge{From: "/n", To: "/n/ravens.md", Kind: "contains"})
		w.Edge(stream.Edge{From: "/n", To: "/n/bread.md", Kind: "contains"})
		w.Sweep()
	})
	s := index1(t, b)
	if len(s.Nodes) != 3 || len(s.Edges) != 2 {
		t.Fatalf("after the first run: %d nodes, %d edges", len(s.Nodes), len(s.Edges))
	}
	if got := s.Runs["src"]; got.Embedded != 2 || got.Changed != 3 {
		t.Errorf("first run = %+v", got)
	}
	if c := s.Nodes["/n/ravens.md"].Chunks[0]; c.Line != 1 || c.EndLine != 3 || c.Prefix != "Ravens" {
		t.Errorf("chunk = %+v", c)
	}

	// Ingest is what is under test: with the walk on, the directory both
	// documents hang off would rank first.
	hits, _, err := search.Run(context.Background(), b, "sourdough starter", search.Options{K: 5, NoGraph: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Node.ID != "/n/bread.md" {
		t.Fatalf("hits = %+v", hits)
	}
	hits, _, _ = search.Run(context.Background(), b, "sourdough starter", search.Options{K: 5, Filters: map[string]string{"topic": "myth"}})
	for _, h := range hits {
		if h.Node.ID == "/n/bread.md" {
			t.Errorf("the filter let %s through", h.Node.ID)
		}
	}

	// The same stream again is no work; a stream without bread sweeps it away,
	// edges included.
	if s = index1(t, b); s.Runs["src"].Embedded != 0 || s.Runs["src"].Changed != 0 {
		t.Errorf("an unchanged re-run = %+v", s.Runs["src"])
	}
	writeStream(t, file, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "/n/ravens.md", Kind: "document", Text: "# Ravens\n\nHuginn and Muninn fly over the world."})
		w.Node(stream.Node{ID: "/n", Kind: "directory"})
		w.Edge(stream.Edge{From: "/n", To: "/n/ravens.md", Kind: "contains"})
		w.Sweep()
	})
	if s = index1(t, b); len(s.Nodes) != 2 || len(s.Edges) != 1 || s.Runs["src"].Deleted != 1 {
		t.Errorf("after the sweep: %d nodes, %d edges, run %+v", len(s.Nodes), len(s.Edges), s.Runs["src"])
	}
}

func TestCursorIsStoredAndHandedBack(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "connector.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"[$MUNINN_CURSOR]\" >> "+dir+"/seen\n"+
		`echo '{"node":{"id":"a","text":"alpha"}}'`+"\n"+`echo '{"cursor":{"value":"seq:7"}}'`+"\n"), 0o755)
	b := newBank(t, "")
	b.Connectors[0].Command = []string{script}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	index1(t, b)
	if s := index1(t, b); s.Cursors["src"] != "seq:7" {
		t.Errorf("cursor = %q", s.Cursors["src"])
	}
	seen, _ := os.ReadFile(dir + "/seen")
	if got := strings.Split(strings.TrimSpace(string(seen)), "\n"); len(got) != 2 || got[0] != "[]" || got[1] != "[seq:7]" {
		t.Errorf("the connector was handed %q", got)
	}
}

func TestUnknownChunkerFailsTheRun(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	b := newBank(t, file)
	writeStream(t, file, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "a", Text: "alpha", Chunker: "cobol"})
	})
	err := index.Run(context.Background(), b, index.Options{})
	if err == nil || !strings.Contains(err.Error(), "cobol") {
		t.Fatalf("err = %v", err)
	}
	if s, _ := index.LoadState(b.IndexDir()); len(s.Nodes) != 0 {
		t.Errorf("a refused stream committed %d nodes", len(s.Nodes))
	}
}

func TestRemoveConnectorNodes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	b := newBank(t, file)
	writeStream(t, file, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "a", Text: "alpha"})
		w.Edge(stream.Edge{From: "a", To: "b", Kind: "links"})
	})
	index1(t, b)
	removed, err := index.RemoveConnectorNodes(b, "src")
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
	if s, _ := index.LoadState(b.IndexDir()); len(s.Nodes) != 0 || len(s.Edges) != 0 {
		t.Errorf("left %d nodes, %d edges", len(s.Nodes), len(s.Edges))
	}
}

// Two connectors: one states a file, the other an edge to it. Removing the
// first leaves the second's edge dangling, and it holds again when the file
// comes back, though the second connector never repeats it.
func TestDeleteKeepsAnotherConnectorsEdges(t *testing.T) {
	dir := t.TempDir()
	files, turns := filepath.Join(dir, "files.jsonl"), filepath.Join(dir, "turns.jsonl")
	b := newBank(t, files)
	b.Connectors = append(b.Connectors, bank.Connector{Name: "sessions", Command: []string{"cat", turns}})
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	writeStream(t, files, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "/r/a.go", Text: "package a"})
		w.Node(stream.Node{ID: "/r", Kind: "directory"})
		w.Edge(stream.Edge{From: "/r", To: "/r/a.go", Kind: "contains"})
		w.Sweep()
	})
	writeStream(t, turns, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "turn:1", Text: "edit a.go"})
		w.Edge(stream.Edge{From: "turn:1", To: "/r/a.go", Kind: "edits"})
	})
	if s := index1(t, b); len(s.Edges) != 2 {
		t.Fatalf("%d edges", len(s.Edges))
	}
	if _, err := index.RemoveConnectorNodes(b, "src"); err != nil {
		t.Fatal(err)
	}
	s, _ := index.LoadState(b.IndexDir())
	if len(s.Edges) != 1 || s.Nodes["/r/a.go"] != nil {
		t.Fatalf("after removing the files: %d edges", len(s.Edges))
	}
	for _, e := range s.Edges {
		if e.Kind != "edits" || e.Connector != "sessions" {
			t.Errorf("the edge left is %+v", e)
		}
	}
}
