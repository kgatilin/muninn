package ui

import (
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/index"
)

// A branch holds two sessions: one with its messages, one compacted onto its
// own node. Both reach the entity; the message's reply is a part, not reached.
func TestReach(t *testing.T) {
	text := []index.Chunk{{Text: "x"}}
	state := &index.State{Nodes: map[string]*index.Node{}, Edges: map[string]*index.Edge{}}
	for id, n := range map[string]*index.Node{
		"branch:b": {Kind: "branch"}, "session:1": {Kind: "session"}, "session:2": {Kind: "session"},
		"message:1": {Kind: "message", Chunks: text}, "reply:1": {Kind: "reply", Chunks: text},
		"entity:bus": {Kind: "entity", Chunks: text}, "rule:r": {Kind: "rule", Chunks: text},
	} {
		n.ID = id
		state.Nodes[id] = n
	}
	for _, e := range []index.Edge{
		{From: "session:1", To: "branch:b", Kind: "on"}, {From: "session:2", To: "branch:b", Kind: "on"},
		{From: "message:1", To: "session:1", Kind: "in"}, {From: "reply:1", To: "session:1", Kind: "in"},
		{From: "reply:1", To: "message:1", Kind: "answers"},
		{From: "message:1", To: "entity:bus", Kind: "mentions"}, {From: "reply:1", To: "rule:r", Kind: "states"},
		{From: "session:2", To: "entity:bus", Kind: "mentions"},
	} {
		state.Edges[e.From+e.To+e.Kind] = &e
	}
	snap := newSnapshot(state, time.Time{})

	parts, groups := snap.reach("branch:b")
	if parts != 4 || len(groups) != 2 || groups[1].Neighbours[0].Text != "x" {
		t.Fatalf("parts %d, groups %+v", parts, groups)
	}
	if g := groups[0]; g.Kind != "mentions" || g.NodeKind != "entity" || g.Total != 1 || g.Neighbours[0].ID != "entity:bus" || g.Neighbours[0].Weight != 2 {
		t.Errorf("mentions = %+v", g)
	}
	if g := groups[1]; g.Kind != "states" || g.Neighbours[0].ID != "rule:r" {
		t.Errorf("states = %+v", g)
	}
	if got := snap.parts("session:1"); len(got) != 3 || !got["reply:1"] {
		t.Errorf("parts of session:1 = %v", got)
	}
	// A rule is its only part, and stands among what states it.
	if parts, groups := snap.reach("rule:r"); parts != 0 || len(groups) != 1 || groups[0].Dir != "in" || groups[0].Neighbours[0].ID != "reply:1" {
		t.Errorf("rule:r: parts %d, groups %+v", parts, groups)
	}
	// The compacted session reaches by its own edges.
	if parts, groups := snap.reach("session:2"); parts != 0 || len(groups) != 2 || groups[0].Neighbours[0].ID != "entity:bus" {
		t.Errorf("session:2: parts %d, groups %+v", parts, groups)
	}
}
