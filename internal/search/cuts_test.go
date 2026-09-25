package search_test

import (
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/stream"
)

// Three notes that share no words, joined only through one tag, and bread
// linked to oven directly.
func tagged(w *stream.Writer) {
	notes(w)
	w.Node(stream.Node{ID: "tag:all", Kind: "tag"})
	for _, doc := range []string{"bread", "oven", "ravens"} {
		w.Edge(stream.Edge{From: doc, To: "tag:all", Kind: "tagged"})
	}
	w.Edge(stream.Edge{From: "bread", To: "oven", Kind: "links"})
}

func TestDegreeCapTakesAHubsEdgesOutOfTheWalk(t *testing.T) {
	for _, tc := range []struct {
		cap                  int
		throughHub, hubFound bool
	}{{0, true, true}, {3, true, true}, {2, false, false}} {
		b := indexed(t, func(b *bank.Bank) {
			oneSeed(b)
			if tc.cap > 0 {
				b.Search.DegreeCap = map[string]int{"tagged": tc.cap}
			}
		}, tagged)
		hits := run(t, b, "sourdough starter", search.Options{K: 5})
		if h := find(hits, "ravens"); h == nil || (h.Mass > 0) != tc.throughHub {
			t.Errorf("cap %d: ravens, reachable only through the tag: %+v", tc.cap, h)
		}
		if (find(hits, "tag:all") != nil) != tc.hubFound {
			t.Errorf("cap %d: hits %v", tc.cap, ids(hits))
		}
		// The cap is per kind: the links edge still carries mass.
		if h := find(hits, "oven"); h == nil || h.Mass <= 0 {
			t.Errorf("cap %d: oven = %+v", tc.cap, h)
		}
	}
}

func TestMutedNodeIsNeitherCrossedNorReturned(t *testing.T) {
	b := indexed(t, func(b *bank.Bank) {
		oneSeed(b)
		b.Search.Mute = []string{"tag:all"}
	}, tagged)
	hits := run(t, b, "sourdough starter", search.Options{K: 5})
	if find(hits, "tag:all") != nil {
		t.Errorf("the muted tag is a hit: %v", ids(hits))
	}
	if h := find(hits, "ravens"); h == nil || h.Mass != 0 {
		t.Errorf("mass crossed the muted tag: %+v", h)
	}
	if h := find(hits, "oven"); h == nil || h.Mass <= 0 {
		t.Errorf("oven = %+v", h)
	}

	// A muted node that matches the text is not a seed and not a hit, with the
	// walk or without it.
	b = indexed(t, func(b *bank.Bank) {
		oneSeed(b)
		b.Search.Mute = []string{"bread"}
	}, tagged)
	for _, o := range []search.Options{{K: 5}, {K: 5, NoGraph: true}} {
		if hits := run(t, b, "sourdough starter", o); find(hits, "bread") != nil || len(hits) == 0 {
			t.Errorf("%+v: hits %v", o, ids(hits))
		}
	}
}
