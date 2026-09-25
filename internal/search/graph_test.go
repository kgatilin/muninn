package search_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/stream"
)

// A connector is `cat <file>`, as in the index tests; the noop embedder leaves
// the ranking to BM25.
func indexed(t *testing.T, set func(*bank.Bank), write func(w *stream.Writer)) *bank.Bank {
	t.Helper()
	t.Setenv("MUNINN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	write(w)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bank.New("test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Set("embedder", "noop"); err != nil {
		t.Fatal(err)
	}
	b.Connectors = []bank.Connector{{Name: "src", Command: []string{"cat", file}}}
	if set != nil {
		set(b)
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := index.Run(context.Background(), b, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return b
}

func run(t *testing.T, b *bank.Bank, query string, o search.Options) []search.Hit {
	t.Helper()
	hits, _, err := search.Run(context.Background(), b, query, o)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func ids(hits []search.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Node.ID
	}
	return out
}

func find(hits []search.Hit, id string) *search.Hit {
	for i := range hits {
		if hits[i].Node.ID == id {
			return &hits[i]
		}
	}
	return nil
}

func notes(w *stream.Writer) {
	w.Node(stream.Node{ID: "bread", Kind: "document", Text: "Sourdough needs a starter and patience."})
	w.Node(stream.Node{ID: "oven", Kind: "document", Text: "A cast iron pot holds heat evenly."})
	w.Node(stream.Node{ID: "ravens", Kind: "document", Text: "Huginn and Muninn fly over the world."})
}

func oneSeed(b *bank.Bank) { b.Search.Seeds = 1 }

// The noop embedder ranks every chunk, so every text node has a fused score;
// one seed keeps the rest of the ranking the walk's.
func TestWalkPullsInANeighbourWithNoSharedWords(t *testing.T) {
	b := indexed(t, oneSeed, func(w *stream.Writer) {
		notes(w)
		w.Edge(stream.Edge{From: "oven", To: "bread", Kind: "links"})
	})
	hits := run(t, b, "sourdough starter", search.Options{K: 5})
	if fmt.Sprint(ids(hits)) != "[bread oven ravens]" {
		t.Fatalf("hits = %v", ids(hits))
	}
	if h := hits[0]; !h.Seed || h.Score <= 0 || h.Mass <= 0 {
		t.Errorf("the seed = %+v", h)
	}
	if h := hits[1]; h.Seed || h.Mass <= 0 {
		t.Errorf("the neighbour = %+v", h)
	}
	if h := hits[2]; h.Seed || h.Mass != 0 {
		t.Errorf("the unlinked node = %+v", h)
	}

	var out bytes.Buffer
	search.Render{}.Text(&out, "sourdough starter", hits)
	if got := out.String(); !strings.HasPrefix(got, "1 bread") || !strings.Contains(got, "\n~2 oven") || !strings.Contains(got, "\n3 ravens") {
		t.Errorf("text:\n%s", got)
	}

	for _, h := range run(t, b, "sourdough starter", search.Options{K: 5, NoGraph: true}) {
		if h.Seed || h.Mass != 0 {
			t.Errorf("--no-graph walked: %+v", h)
		}
	}
	// Filters hold for seeds and for what the walk reaches.
	if hits := run(t, b, "sourdough starter", search.Options{K: 5, Filters: map[string]string{"topic": "food"}}); len(hits) != 0 {
		t.Errorf("filtered: %v", ids(hits))
	}
}

// The same graph as above with the edge to ravens instead, and a links edge
// the bank has switched off.
func TestZeroEdgeWeightRemovesAKind(t *testing.T) {
	b := indexed(t, func(b *bank.Bank) {
		oneSeed(b)
		b.Search.EdgeWeights = map[string]float64{"links": 0}
	}, func(w *stream.Writer) {
		notes(w)
		w.Edge(stream.Edge{From: "bread", To: "oven", Kind: "links"})
		w.Edge(stream.Edge{From: "bread", To: "ravens", Kind: "cites"})
	})
	hits := run(t, b, "sourdough starter", search.Options{K: 5})
	if fmt.Sprint(ids(hits)) != "[bread ravens oven]" || hits[2].Mass != 0 {
		t.Errorf("hits = %v, oven = %+v", ids(hits), hits[2])
	}
}

func TestNoEdgesRanksAsFusion(t *testing.T) {
	for _, seeds := range []int{2, 10} {
		b := indexed(t, func(b *bank.Bank) { b.Search.Seeds = seeds }, func(w *stream.Writer) {
			w.Node(stream.Node{ID: "a", Text: "starter starter starter, the sourdough starter"})
			w.Node(stream.Node{ID: "b", Text: "a starter motor for an engine, among many other words about engines"})
			w.Node(stream.Node{ID: "c", Text: "sourdough"})
			w.Node(stream.Node{ID: "d", Text: "ravens"})
		})
		with := run(t, b, "sourdough starter", search.Options{K: 5})
		without := run(t, b, "sourdough starter", search.Options{K: 5, NoGraph: true})
		if len(with) != 4 || fmt.Sprint(ids(with)) != fmt.Sprint(ids(without)) {
			t.Errorf("seeds=%d: with %v, without %v", seeds, ids(with), ids(without))
		}
		for i, h := range with {
			if h.Seed != (i < seeds) || h.Score != without[i].Score {
				t.Errorf("seeds=%d: hit %d = %+v", seeds, i, h)
			}
		}
	}
}

// Three tags every document hangs off gather more mass than any document.
func TestStructuralNodesAreCapped(t *testing.T) {
	b := indexed(t, nil, func(w *stream.Writer) {
		for i := range 6 {
			doc := fmt.Sprintf("/n/doc%d.md", i)
			w.Node(stream.Node{ID: doc, Kind: "document", Text: fmt.Sprintf("Sourdough starter, note %d.", i)})
			for _, tag := range []string{"tag:a", "tag:b", "tag:c"} {
				w.Edge(stream.Edge{From: doc, To: tag, Kind: "tagged"})
			}
		}
		for _, tag := range []string{"tag:a", "tag:b", "tag:c"} {
			w.Node(stream.Node{ID: tag, Kind: "tag", Attrs: map[string]string{"name": "baking", "colour": "red"}})
		}
	})
	hits := run(t, b, "sourdough starter", search.Options{K: 8})
	structural := 0
	for _, h := range hits {
		if len(h.Node.Chunks) == 0 {
			structural++
		}
	}
	if len(hits) != 8 || structural != 2 || hits[0].Node.Kind != "document" || hits[6].Node.Kind != "tag" {
		t.Fatalf("%d hits, %d structural: %v", len(hits), structural, ids(hits))
	}
	var out bytes.Buffer
	search.Render{}.Text(&out, "sourdough starter", hits)
	if got := out.String(); !strings.Contains(got, "~7 tag:a [tag] colour=red name=baking\n") {
		t.Errorf("text:\n%s", got)
	}

	if hits := run(t, b, "sourdough starter", search.Options{K: 8, Kind: "tag"}); len(hits) != 0 {
		t.Errorf("--kind tag leaves no seeds, got %v", ids(hits))
	}
	if hits := run(t, b, "sourdough starter", search.Options{K: 8, Kind: "document"}); len(hits) != 6 {
		t.Errorf("--kind document: %v", ids(hits))
	}
}
