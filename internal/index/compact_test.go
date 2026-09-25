package index_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/stream"
)

// An old conversation that has been read leaves the graph; what its parts
// were tied to, the session is tied to; and the connector saying it all again
// brings nothing back. A recent one stays whole.
func TestCompact(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	b := newBank(t, file)
	for k, v := range map[string]string{"compact.kinds": "message,reply", "compact.after": "60d"} {
		if _, err := b.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	old, recent := now.AddDate(0, 0, -90), now.AddDate(0, 0, -5)
	writeStream(t, file, func(w *stream.Writer) {
		for _, c := range []struct {
			session string
			at      time.Time
		}{{"old", old}, {"new", recent}} {
			w.Node(stream.Node{ID: "session:" + c.session, Kind: "session"})
			previous := ""
			for _, part := range []string{"message", "reply"} {
				id := part + ":" + c.session
				w.Node(stream.Node{ID: id, Kind: part, Text: part + " of the " + c.session + " session", At: &c.at})
				w.Edge(stream.Edge{From: id, To: "session:" + c.session, Kind: "in"})
				w.Edge(stream.Edge{From: id, To: "/app/main.go", Kind: "edits"})
				if previous != "" {
					w.Edge(stream.Edge{From: previous, To: id, Kind: "next"})
				}
				previous = id
			}
		}
	})
	index1(t, b)

	compact := func(read func(*index.Node) bool) (int, int) {
		t.Helper()
		s, err := index.Open(b, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		owner := index.EnrichConnector("process")
		s.Put(index.NewTextNode("rule:1", "rule", owner, "Tests first.", nil))
		s.PutEdge(&index.Edge{From: "message:old", To: "rule:1", Kind: "states", Weight: 1, Connector: owner})
		s.PutEdge(&index.Edge{From: "reply:old", To: "rule:1", Kind: "states", Weight: 1, Connector: owner})
		nodes, containers := s.Compact(now, read)
		if err := s.SaveState(); err != nil {
			t.Fatal(err)
		}
		return nodes, containers
	}

	if nodes, _ := compact(func(n *index.Node) bool { return n.ID != "reply:old" }); nodes != 0 {
		t.Fatalf("a session with an unread reply lost %d nodes", nodes)
	}
	if nodes, containers := compact(func(*index.Node) bool { return true }); nodes != 2 || containers != 1 {
		t.Fatalf("compacted %d nodes of %d containers, want 2 of 1", nodes, containers)
	}

	s := index1(t, b) // the connector emits the old session again
	for _, id := range []string{"message:old", "reply:old"} {
		if s.Nodes[id] != nil {
			t.Errorf("%s is back", id)
		}
	}
	for _, id := range []string{"session:old", "message:new", "reply:new", "rule:1"} {
		if s.Nodes[id] == nil {
			t.Errorf("%s is gone", id)
		}
	}
	states := s.Edges[index.Edge{From: "session:old", To: "rule:1", Kind: "states"}.Key()]
	if states == nil || states.Weight != 2 {
		t.Errorf("the session states the rule with %+v, want weight 2", states)
	}
	if s.Edges[index.Edge{From: "session:old", To: "/app/main.go", Kind: "edits"}.Key()] == nil {
		t.Error("the session does not carry the edit of its parts")
	}
	for _, e := range s.Edges {
		if s.Compacted[e.From] != "" || s.Compacted[e.To] != "" {
			t.Errorf("edge %s -%s-> %s touches a compacted node", e.From, e.Kind, e.To)
		}
	}

	if _, err := index.RemoveConnectorNodes(b, "src"); err != nil {
		t.Fatal(err)
	}
	if s = index1(t, b); s.Nodes["message:old"] == nil {
		t.Error("removing the connector did not forget what was compacted")
	}
}
