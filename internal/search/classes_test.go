package search_test

import (
	"fmt"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/stream"
)

// Many turns of talk about the starter and one guide on it: ranked together the
// talk takes every place, ranked by class the guide is answered first.
func TestAClassIsRankedOnItsOwn(t *testing.T) {
	b := indexed(t, func(b *bank.Bank) {
		for _, kv := range [][2]string{{"class.guide.paths", "**/GUIDE.md"}, {"class.talk.kinds", "message"}} {
			if _, err := b.Set(kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
		}
	}, func(w *stream.Writer) {
		for i := range 6 {
			w.Node(stream.Node{ID: fmt.Sprintf("turn:%d", i), Kind: "message", Text: "the sourdough starter, the sourdough starter again"})
		}
		w.Node(stream.Node{ID: "/kitchen/GUIDE.md", Kind: "document", Text: "Feed the sourdough starter daily."})
	})
	hits := run(t, b, "sourdough starter", search.Options{K: 2, NoGraph: true})
	if len(hits) != 3 || hits[0].Node.ID != "/kitchen/GUIDE.md" || hits[0].Class != "guide" || hits[1].Class != "talk" || hits[2].Class != "talk" {
		t.Errorf("hits %v", ids(hits))
	}
}

// Asked from a node and with no text, the answer is what stands around it.
func TestFromANodeTheAnswerIsWhatStandsAroundIt(t *testing.T) {
	b := indexed(t, nil, func(w *stream.Writer) {
		notes(w)
		w.Node(stream.Node{ID: "branch:baking", Kind: "branch"})
		w.Edge(stream.Edge{From: "bread", To: "branch:baking", Kind: "on"})
		w.Edge(stream.Edge{From: "bread", To: "oven", Kind: "links"})
	})
	hits := run(t, b, "", search.Options{K: 5, From: "branch:baking"})
	if len(hits) == 0 || hits[0].Node.ID != "bread" || find(hits, "ravens") != nil || find(hits, "branch:baking") != nil {
		t.Errorf("hits %v", ids(hits))
	}
	if _, _, err := search.Run(t.Context(), b, "", search.Options{From: "branch:none"}); err == nil {
		t.Error("a node the bank does not hold was walked from")
	}
}
