package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/internal/ui"
	"github.com/kgatilin/muninn/internal/usage"
	"github.com/kgatilin/muninn/stream"
)

// The bank is indexed from `cat <file>`, as in the index tests: a directory
// holding three documents, one of which links to another, and a turn.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("MUNINN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	w.Node(stream.Node{ID: "/n", Kind: "directory"})
	w.Node(stream.Node{ID: "/n/ravens.md", Kind: "document", Text: "# Ravens\n\nHuginn and Muninn fly over the world.", Attrs: map[string]string{"topic": "myth"}})
	w.Node(stream.Node{ID: "/n/bread.md", Kind: "document", Text: "# Bread\n\nSourdough needs a starter and patience."})
	w.Node(stream.Node{ID: "/n/odin.md", Kind: "document", Text: "# Odin\n\nThe one-eyed god keeps two birds.", Attrs: map[string]string{"title": "Odin, the Allfather"}})
	w.Node(stream.Node{ID: "turn:cc:abc:1", Kind: "turn", Text: "What does a sourdough starter need besides flour, water and a great deal of patience from its keeper?"})
	for _, doc := range []string{"ravens", "bread", "odin"} {
		w.Edge(stream.Edge{From: "/n", To: "/n/" + doc + ".md", Kind: "contains"})
	}
	w.Edge(stream.Edge{From: "/n/odin.md", To: "/n/ravens.md", Kind: "links"})
	w.Edge(stream.Edge{From: "/n/odin.md", To: "/n/missing.md", Kind: "links"})
	w.Sweep()
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
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := index.Run(context.Background(), b, index.Options{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ui.NewServer().Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string, status int, into any) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != status {
		t.Fatalf("GET %s = %d, want %d", path, res.StatusCode, status)
	}
	if into != nil {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
}

func TestBanks(t *testing.T) {
	srv := newServer(t)
	var got struct {
		Banks []struct {
			Name, Embedder, Chunker string
			Counts                  struct{ Nodes, TextNodes, Chunks, Edges int }
			Connectors              []struct {
				Name    string
				Command []string
				Nodes   int
				LastRun string `json:"last_run"`
			}
		}
	}
	get(t, srv, "/api/banks", 200, &got)
	if len(got.Banks) != 1 {
		t.Fatalf("banks = %+v", got.Banks)
	}
	b := got.Banks[0]
	if b.Name != "test" || b.Embedder != "noop" || b.Chunker != "text" {
		t.Errorf("bank = %+v", b)
	}
	if b.Counts.Nodes != 5 || b.Counts.Edges != 5 || b.Counts.Chunks != 4 {
		t.Errorf("counts = %+v", b.Counts)
	}
	if len(b.Connectors) != 1 || b.Connectors[0].Nodes != 5 || b.Connectors[0].LastRun == "" || b.Connectors[0].Command[0] != "cat" {
		t.Errorf("connectors = %+v", b.Connectors)
	}
}

type graphPayload struct {
	Nodes []struct {
		ID, Kind, Label string
		Degree, Chunks  int
	}
	Edges      []struct{ From, To, Kind string }
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	Truncated  bool           `json:"truncated"`
	NodeKinds  map[string]int `json:"node_kinds"`
	EdgeKinds  map[string]int `json:"edge_kinds"`
}

func (g graphPayload) ids() string {
	ids := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		ids[i] = n.ID
	}
	return strings.Join(ids, " ")
}

func TestGraph(t *testing.T) {
	srv := newServer(t)
	var g graphPayload
	get(t, srv, "/api/banks/test/graph", 200, &g)
	if len(g.Nodes) != 5 || g.TotalNodes != 5 || g.Truncated {
		t.Fatalf("nodes = %s, total %d, truncated %v", g.ids(), g.TotalNodes, g.Truncated)
	}
	// The edge to a node the bank does not hold is not drawn and not counted.
	if len(g.Edges) != 4 || g.TotalEdges != 4 || g.EdgeKinds["contains"] != 3 || g.EdgeKinds["links"] != 1 {
		t.Errorf("edges = %+v, kinds %v", g.Edges, g.EdgeKinds)
	}
	if g.NodeKinds["document"] != 3 || g.NodeKinds["directory"] != 1 || g.NodeKinds["turn"] != 1 {
		t.Errorf("node kinds = %v", g.NodeKinds)
	}
	labels := map[string]string{}
	for _, n := range g.Nodes {
		labels[n.ID] = n.Label
	}
	for id, want := range map[string]string{
		"/n/ravens.md":  "ravens.md",
		"/n/odin.md":    "Odin, the Allfather",
		"turn:cc:abc:1": "What does a sourdough starter need besides flour, water and …",
	} {
		if labels[id] != want {
			t.Errorf("label of %s = %q, want %q", id, labels[id], want)
		}
	}
	if first := g.Nodes[0]; first.ID != "/n" || first.Degree != 3 || first.Chunks != 0 {
		t.Errorf("highest degree first: got %+v", first)
	}

	get(t, srv, "/api/banks/test/graph?kind=document", 200, &g)
	if len(g.Nodes) != 3 || len(g.Edges) != 1 {
		t.Errorf("kind=document: nodes %s, edges %+v", g.ids(), g.Edges)
	}
	get(t, srv, "/api/banks/nope/graph", 404, nil)
	get(t, srv, "/api/banks/test/graph?limit=x", 400, nil)
}

func TestGraphLimit(t *testing.T) {
	srv := newServer(t)
	var g graphPayload
	get(t, srv, "/api/banks/test/graph?limit=2", 200, &g)
	if g.ids() != "/n /n/odin.md" || !g.Truncated || len(g.Edges) != 1 {
		t.Errorf("limit=2: nodes %s, truncated %v, edges %+v", g.ids(), g.Truncated, g.Edges)
	}
	// `with` adds the node and its neighbours over the limit.
	g = graphPayload{}
	get(t, srv, "/api/banks/test/graph?limit=1&with="+url.QueryEscape("/n/ravens.md")+"&with=turn:cc:abc:1&with=absent", 200, &g)
	if g.ids() != "/n /n/odin.md /n/ravens.md turn:cc:abc:1" || !g.Truncated || len(g.Edges) != 3 {
		t.Errorf("limit=1 with: nodes %s, edges %+v", g.ids(), g.Edges)
	}
	g = graphPayload{}
	get(t, srv, "/api/banks/test/graph?limit=0", 200, &g)
	if len(g.Nodes) != 5 || g.Truncated {
		t.Errorf("limit=0: nodes %s", g.ids())
	}
}

func TestNodesAndAround(t *testing.T) {
	srv := newServer(t)
	var got struct {
		Nodes []struct{ ID, Label string }
		Total int
	}
	get(t, srv, "/api/banks/test/nodes?kind=document", 200, &got)
	if got.Total != 3 || len(got.Nodes) != 3 {
		t.Fatalf("documents = %+v", got)
	}
	get(t, srv, "/api/banks/test/nodes?kind=document&q=allfather", 200, &got)
	if got.Total != 1 || got.Nodes[0].ID != "/n/odin.md" {
		t.Fatalf("q = %+v", got)
	}
	// A document's surroundings are what its own edges reach: its directory
	// and the document it links to, not the directory's other documents.
	var g graphPayload
	get(t, srv, "/api/banks/test/graph?around="+url.QueryEscape("/n/odin.md"), 200, &g)
	if len(g.Nodes) != 3 || strings.Contains(g.ids(), "bread") {
		t.Fatalf("around = %s", g.ids())
	}
}

func TestNode(t *testing.T) {
	srv := newServer(t)
	var n struct {
		ID, Kind, Label string
		Attrs           map[string]string
		Chunks          []struct {
			Prefix, Text string
			Lines        []int
		}
		Edges []struct {
			Kind, Dir  string
			Neighbours []struct{ ID, Kind, Label string }
		}
	}
	get(t, srv, "/api/banks/test/node?id="+url.QueryEscape("/n/ravens.md"), 200, &n)
	if n.Kind != "document" || n.Attrs["topic"] != "myth" {
		t.Errorf("node = %+v", n)
	}
	if len(n.Chunks) != 1 || n.Chunks[0].Prefix != "Ravens" || !strings.Contains(n.Chunks[0].Text, "Huginn") || len(n.Chunks[0].Lines) != 2 {
		t.Errorf("chunks = %+v", n.Chunks)
	}
	if len(n.Edges) != 2 || n.Edges[0].Dir != "in" || n.Edges[0].Kind != "contains" || n.Edges[1].Kind != "links" {
		t.Fatalf("edges = %+v", n.Edges)
	}
	if nb := n.Edges[1].Neighbours; len(nb) != 1 || nb[0].ID != "/n/odin.md" || nb[0].Label != "Odin, the Allfather" || nb[0].Kind != "document" {
		t.Errorf("links neighbours = %+v", nb)
	}
	get(t, srv, "/api/banks/test/node?id=absent", 404, nil)
}

func TestSearch(t *testing.T) {
	srv := newServer(t)
	var got struct {
		Hits []struct {
			ID, Label, Heading, Snippet string
			Lines                       []int
			Score, Mass                 float64
			Seed                        bool
		}
	}
	get(t, srv, "/api/banks/test/search?q=huginn&k=5", 200, &got)
	if len(got.Hits) == 0 {
		t.Fatal("no hits")
	}
	first := got.Hits[0]
	if first.ID != "/n/ravens.md" || !first.Seed || first.Mass <= 0 || first.Score <= 0 || first.Heading != "Ravens" || !strings.Contains(first.Snippet, "Huginn") {
		t.Errorf("first hit = %+v", first)
	}
	// The noop embedder ranks every chunk, so every text node is a seed; the
	// directory has no text and is there through the walk alone.
	reached := false
	for _, h := range got.Hits {
		if h.ID == "/n" {
			reached = !h.Seed && h.Score == 0 && h.Mass > 0
		}
	}
	if !reached {
		t.Errorf("/n was not reached through the graph: %+v", got.Hits)
	}

	get(t, srv, "/api/banks/test/search?q=huginn&nograph=1", 200, &got)
	if len(got.Hits) != 4 || got.Hits[0].ID != "/n/ravens.md" {
		t.Errorf("nograph hits = %+v", got.Hits)
	}
	for _, h := range got.Hits {
		if h.Mass != 0 || h.Seed {
			t.Errorf("nograph: %s has mass %g, seed %v", h.ID, h.Mass, h.Seed)
		}
	}
	get(t, srv, "/api/banks/test/search?q=", 400, nil)
}

// The query endpoint answers with what `muninn search` prints for the same
// options, which is what lets the command hand its query to a running server.
func TestQuery(t *testing.T) {
	srv := newServer(t)
	b, err := bank.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	q := ui.Query{
		Text:    "huginn",
		Options: search.Options{K: 3, Kind: "document", Filters: map[string]string{"topic": "myth"}},
		Render:  search.Render{Chars: 80, Attrs: []string{"topic"}},
	}
	back, err := ui.ParseQuery(q.Values())
	if err != nil || !reflect.DeepEqual(back, q) {
		t.Fatalf("ParseQuery(Values()) = %+v, %v; want %+v", back, err, q)
	}
	hits, _, err := search.Run(context.Background(), b, q.Text, q.Options)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	q.Render.Text(&want, q.Text, hits)

	resp, err := http.Get(srv.URL + "/api/banks/test/query?" + q.Values().Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != want.String() || !strings.Contains(string(got), "ravens.md") {
		t.Errorf("status %d, body:\n%s\nwant:\n%s", resp.StatusCode, got, want.String())
	}
	get(t, srv, "/api/banks/test/query?q=huginn", 400, nil)
}

func TestStatic(t *testing.T) {
	srv := newServer(t)
	for path, want := range map[string]string{
		"/":                   "text/html",
		"/app.js":             "text/javascript",
		"/vendor/d3-force.js": "text/javascript",
		"/style.css":          "text/css",
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if ct := res.Header.Get("Content-Type"); res.StatusCode != 200 || !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s = %d %q, want %s", path, res.StatusCode, ct, want)
		}
	}
}

func TestUsage(t *testing.T) {
	srv := newServer(t)
	var got struct {
		All, Bank, Today struct {
			Calls, In int
			Cost      float64
		}
		ByPurpose []struct{ Key string } `json:"by_purpose"`
		ByModel   []struct{ Key string } `json:"by_model"`
	}
	get(t, srv, "/api/banks/test/usage", 200, &got)
	if got.All.Calls != 0 {
		t.Fatalf("usage before any paid call: %+v", got)
	}
	home, _ := bank.Home()
	for _, name := range []string{"test", "other"} {
		if err := usage.Append(home, name, usage.Embed, "vertex:gemini-embedding-001", usage.Tokens{In: 1_000_000, Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	get(t, srv, "/api/banks/test/usage", 200, &got)
	if got.All.Calls != 2 || got.Bank.Calls != 1 || got.Today.Calls != 1 || got.Bank.Cost != 0.15 ||
		len(got.ByPurpose) != 1 || got.ByPurpose[0].Key != usage.Embed || len(got.ByModel) != 1 {
		t.Fatalf("%+v", got)
	}
}

// A kind the page switched off takes none of the limit's places.
func TestGraphHide(t *testing.T) {
	srv := newServer(t)
	var got graphPayload
	get(t, srv, "/api/banks/test/graph?limit=1", 200, &got)
	if got.ids() != "/n" {
		t.Fatalf("the top of the bank = %s", got.ids())
	}
	get(t, srv, "/api/banks/test/graph?limit=5&hide=directory&hide=document", 200, &got)
	if got.ids() != "turn:cc:abc:1" {
		t.Errorf("with the kinds above it hidden = %s", got.ids())
	}
}
