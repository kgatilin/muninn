package export

import (
	"bytes"
	"encoding/xml"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/index"
)

type doc struct {
	Keys []struct {
		ID   string `xml:"id,attr"`
		For  string `xml:"for,attr"`
		Name string `xml:"attr.name,attr"`
	} `xml:"key"`
	Graph struct {
		EdgeDefault string `xml:"edgedefault,attr"`
		Nodes       []elem `xml:"node"`
		Edges       []elem `xml:"edge"`
	} `xml:"graph"`
}

type elem struct {
	ID     string `xml:"id,attr"`
	Source string `xml:"source,attr"`
	Target string `xml:"target,attr"`
	Data   []struct {
		Key   string `xml:"key,attr"`
		Value string `xml:",chardata"`
	} `xml:"data"`
}

// attrs reads an element's data through the declared keys, and fails on a key
// the file does not declare for that scope.
func (d doc) attrs(t *testing.T, scope string, e elem) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, item := range e.Data {
		name := ""
		for _, k := range d.Keys {
			if k.ID == item.Key && k.For == scope {
				name = k.Name
			}
		}
		if name == "" {
			t.Fatalf("%s %s uses key %q, which is not declared", scope, e.ID, item.Key)
		}
		out[name] = item.Value
	}
	return out
}

func state() *index.State {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := &index.State{Nodes: map[string]*index.Node{}, Edges: map[string]*index.Edge{}}
	for _, n := range []*index.Node{
		{ID: "/notes/a&b.md", Kind: "document", Connector: "notes", At: at, Chunks: []index.Chunk{{Text: "one"}, {Text: "two"}}},
		{ID: "/notes", Kind: "directory", Connector: "notes"},
		{ID: "entity:ravens", Kind: "entity", Connector: "~semantic", Attrs: map[string]string{"name": "Ravens <two>"}},
	} {
		s.Nodes[n.ID] = n
	}
	for i, e := range []*index.Edge{
		{From: "/notes", To: "/notes/a&b.md", Kind: "contains", Connector: "notes"},
		{From: "/notes/a&b.md", To: "entity:ravens", Kind: "mentions", Weight: 0.5, Connector: "~semantic"},
		{From: "/notes/a&b.md", To: "/notes/gone.md", Kind: "links", Connector: "notes"},
		{From: "session:1", To: "/notes/a&b.md", Kind: "reads", Connector: "sessions"},
	} {
		s.Edges[string(rune('a'+i))] = e
	}
	return s
}

func TestGraphMLRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	res, err := GraphML(&buf, state(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Nodes: 3, Edges: 2, Dangling: 2}) {
		t.Errorf("result %+v", res)
	}
	var d doc
	if err := xml.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	declared := map[string]bool{}
	for _, k := range d.Keys {
		declared[k.For+"."+k.Name] = true
	}
	for _, want := range []string{"node.id", "node.kind", "node.label", "node.connector", "node.chunks", "node.degree", "node.at", "edge.kind", "edge.weight", "edge.connector"} {
		if !declared[want] {
			t.Errorf("key %s is not declared", want)
		}
	}
	if d.Graph.EdgeDefault != "directed" || len(d.Graph.Nodes) != 3 || len(d.Graph.Edges) != 2 {
		t.Fatalf("%s, %d nodes, %d edges", d.Graph.EdgeDefault, len(d.Graph.Nodes), len(d.Graph.Edges))
	}

	byXML := map[string]map[string]string{}
	for _, n := range d.Graph.Nodes {
		byXML[n.ID] = d.attrs(t, "node", n)
	}
	var file, entity map[string]string
	for _, a := range byXML {
		switch a["id"] {
		case "/notes/a&b.md":
			file = a
		case "entity:ravens":
			entity = a
		}
	}
	if file["label"] != "a&b.md" || file["kind"] != "document" || file["chunks"] != "2" || file["degree"] != "2" || file["at"] != "2026-03-01T12:00:00Z" || file["connector"] != "notes" {
		t.Errorf("file node %v", file)
	}
	if entity["label"] != "Ravens <two>" || entity["at"] != "" || entity["degree"] != "1" {
		t.Errorf("entity node %v", entity)
	}
	for _, e := range d.Graph.Edges {
		a := d.attrs(t, "edge", e)
		from, to := byXML[e.Source], byXML[e.Target]
		if from == nil || to == nil {
			t.Fatalf("edge %s joins %s and %s, which are not nodes of the file", e.ID, e.Source, e.Target)
		}
		if a["kind"] == "mentions" && (a["weight"] != "0.5" || a["connector"] != "~semantic" || to["id"] != "entity:ravens") {
			t.Errorf("mentions edge %v to %v", a, to)
		}
	}

	var again bytes.Buffer
	if _, err := GraphML(&again, state(), Options{}); err != nil || !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Errorf("two exports of one state differ (err %v)", err)
	}
}

func TestGraphMLKinds(t *testing.T) {
	var buf bytes.Buffer
	res, err := GraphML(&buf, state(), Options{Kinds: []string{"document", "entity"}, EdgeKinds: []string{"mentions", "contains"}})
	if err != nil {
		t.Fatal(err)
	}
	// contains has lost its directory, which is a choice and not a dangling
	// edge; links and reads were not asked for.
	if res != (Result{Nodes: 2, Edges: 1}) {
		t.Errorf("result %+v", res)
	}
}
