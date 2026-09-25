package semantic

import (
	"math"
	"testing"
)

// unit is a unit vector at the angle whose cosine with the first axis is cos,
// leaning into axis lean.
func unit(cos float64, lean int) []float32 {
	v := make([]float32, 8)
	v[0], v[lean] = float32(cos), float32(math.Sqrt(1-cos*cos))
	return v
}

func TestFoldNames(t *testing.T) {
	vectors := map[string][]float32{
		"agentic reinforcement learning": unit(1, 1),
		"agentic rl":                     unit(0.97, 1), // the same thing, abbreviated
		"inverse reinforcement learning": unit(0.87, 2), // related, and another thing
		"event log":                      unit(0.1, 3),
		"event logs store":               unit(0.1, 3), // within the fold of an entity the layer holds
		"no vector":                      nil,
	}
	voters := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	chosen := map[string]map[string]bool{
		"agentic rl":                     voters("a", "b", "c"),
		"agentic reinforcement learning": voters("d"),
		"inverse reinforcement learning": voters("e", "f"),
		"event logs store":               voters("g"),
		"no vector":                      voters("h"),
	}
	canon := foldNames([]string{"event log"}, chosen, func(name string) []float32 { return vectors[name] })
	for name, want := range map[string]string{
		"agentic rl":                     "agentic rl", // the most chosen is the name
		"agentic reinforcement learning": "agentic rl",
		"inverse reinforcement learning": "inverse reinforcement learning",
		"event logs store":               "event log",
		"no vector":                      "no vector",
	} {
		if canon[name] != want {
			t.Errorf("%q counts under %q, want %q", name, canon[name], want)
		}
	}
}

func TestSweepBelowTheVotes(t *testing.T) {
	state := newTexts(4)
	g := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 100}
	for _, p := range []Patch{
		{Ops: []Op{AddEntity("pair"), AddMention(textID(0), "entity:pair", 0), AddMention(textID(1), "entity:pair", 0)}},
		{Ops: []Op{AddEntity("leaf"), AddMention(textID(2), "entity:leaf", 0)}},
		// One direct mention and two under its topic: three text nodes in all.
		{Ops: []Op{AddEntity("whole"), AddMention(textID(0), "entity:whole", 0), AddMention(textID(1), "entity:whole", 0), AddMention(textID(3), "entity:whole", 0)}},
		{Ops: []Op{InsertIntermediate("entity:whole", "part", textID(0), textID(1))}},
	} {
		if ev, err := g.Propose(p); err != nil || ev.Decision != Accept {
			t.Fatalf("%+v: %v %v", p, ev.Decision, err)
		}
	}
	if swept := g.Layer.Sweep(2); swept != 1 || state.Nodes["entity:leaf"] != nil {
		t.Fatalf("swept %d", swept)
	}
	if state.Nodes["entity:pair"] == nil || state.Nodes["entity:whole"] == nil || state.Nodes["topic:part"] == nil {
		t.Fatal("an entity with its votes went")
	}
}
