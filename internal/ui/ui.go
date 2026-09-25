// Package ui is the local page: a read-only HTTP server over the packages the
// CLI uses. It reads the committed snapshot like `search` does, takes no lock
// and writes nothing.
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kgatilin/muninn/internal/bank"
	embedder "github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/enrich"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/internal/semantic"
	"github.com/kgatilin/muninn/internal/usage"
)

//go:embed static
var static embed.FS

// DefaultLimit is how many nodes a graph payload holds when none is asked for.
const DefaultLimit = 1500

type Server struct {
	mu    sync.Mutex
	cache map[string]*snapshot
}

// snapshot is a bank as a query reads it, with what every request would
// otherwise compute again: the adjacency, the degrees and the kind counts. It
// is good for as long as state.gob keeps its mtime.
type snapshot struct {
	mtime     time.Time
	corpus    *search.Corpus
	state     *index.State
	adjacent  map[string][]*index.Edge
	byDegree  []string // node ids, highest degree first
	nodeKinds map[string]int
	edgeKinds map[string]int
	edges     int // edges with both ends in the bank
}

func NewServer() *Server { return &Server{cache: map[string]*snapshot{}} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/banks", s.banks)
	mux.HandleFunc("GET /api/banks/{bank}/graph", s.graph)
	mux.HandleFunc("GET /api/banks/{bank}/node", s.node)
	mux.HandleFunc("GET /api/banks/{bank}/nodes", s.nodes)
	mux.HandleFunc("GET /api/banks/{bank}/search", s.search)
	mux.HandleFunc("GET /api/banks/{bank}/query", s.query)
	mux.HandleFunc("GET /api/banks/{bank}/usage", s.usage)
	files, _ := fs.Sub(static, "static")
	// The page's files change with the binary and carry no date of their own:
	// the browser asks again each time and does not keep an older page.
	page := http.FileServerFS(files)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		page.ServeHTTP(w, r)
	})
	return mux
}

func (s *Server) load(name string) (*bank.Bank, *snapshot, error) {
	b, err := bank.Load(name)
	if err != nil {
		return nil, nil, err
	}
	mtime := search.Stamp(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap := s.cache[name]; snap != nil && snap.mtime.Equal(mtime) {
		return b, snap, nil
	}
	corpus, err := search.Load(b)
	if err != nil {
		return nil, nil, err
	}
	snap := newSnapshot(corpus.State, mtime)
	snap.corpus = corpus
	s.cache[name] = snap
	return b, snap, nil
}

// LastRun is when the bank's connectors last ran, the latest of them; zero for
// a bank never indexed or one that does not load.
func (s *Server) LastRun(name string) time.Time {
	var last time.Time
	if _, snap, err := s.load(name); err == nil {
		for _, run := range snap.state.Runs {
			if run.At.After(last) {
				last = run.At
			}
		}
	}
	return last
}

// An edge to a node the bank does not hold is left out of the adjacency and of
// every count: the page has nothing to draw at its other end.
func newSnapshot(state *index.State, mtime time.Time) *snapshot {
	snap := &snapshot{mtime: mtime, state: state, adjacent: map[string][]*index.Edge{}, nodeKinds: map[string]int{}, edgeKinds: map[string]int{}}
	for _, e := range state.Edges {
		if state.Nodes[e.From] == nil || state.Nodes[e.To] == nil {
			continue
		}
		snap.edges++
		snap.edgeKinds[e.Kind]++
		snap.adjacent[e.From] = append(snap.adjacent[e.From], e)
		if e.To != e.From {
			snap.adjacent[e.To] = append(snap.adjacent[e.To], e)
		}
	}
	for id, n := range state.Nodes {
		snap.nodeKinds[n.Kind]++
		snap.byDegree = append(snap.byDegree, id)
	}
	sort.Slice(snap.byDegree, func(i, j int) bool {
		a, b := snap.byDegree[i], snap.byDegree[j]
		if da, db := len(snap.adjacent[a]), len(snap.adjacent[b]); da != db {
			return da > db
		}
		return a < b
	})
	return snap
}

type bankInfo struct {
	Name        string             `json:"name"`
	Dir         string             `json:"dir,omitempty"`
	Error       string             `json:"error,omitempty"`
	Embedder    string             `json:"embedder,omitempty"`
	Endpoint    string             `json:"endpoint,omitempty"`
	Chunker     string             `json:"chunker,omitempty"`
	Budgets     map[string]int     `json:"budgets,omitempty"`
	Seeds       int                `json:"seeds,omitempty"`
	EdgeWeights map[string]float64 `json:"edge_weights,omitempty"`
	Counts      counts             `json:"counts"`
	Connectors  []connectorInfo    `json:"connectors"`
	Enrich      []enrich.SetStatus `json:"enrich,omitempty"`
	Layer       layerInfo          `json:"layer"`
	Compacted   int                `json:"compacted,omitempty"`
	IndexEvery  string             `json:"index_every,omitempty"`
}

// layerInfo is the semantic layer and how much of the bank the stage has read.
type layerInfo struct {
	Entities int `json:"entities"`
	Topics   int `json:"topics"`
	Mentions int `json:"mentions"`
	Read     int `json:"read"`
	Pending  int `json:"pending"`
}

type counts struct {
	Nodes     int `json:"nodes"`
	TextNodes int `json:"text_nodes"`
	Chunks    int `json:"chunks"`
	Edges     int `json:"edges"`
	Vectors   int `json:"vectors"`
}

type connectorInfo struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Nodes   int      `json:"nodes"`
	// Kinds is the connector's nodes by kind: how many sessions it has brought.
	Kinds   map[string]int `json:"kinds,omitempty"`
	LastRun *time.Time     `json:"last_run,omitempty"`
	RunErr  string         `json:"run_error,omitempty"`
	Cursor  string         `json:"cursor,omitempty"`
}

// banks is `bank show` for every bank. One that does not load is listed with
// its error and the rest still are.
func (s *Server) banks(w http.ResponseWriter, _ *http.Request) {
	names, err := bank.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]bankInfo, 0, len(names))
	for _, name := range names {
		b, snap, err := s.load(name)
		if err != nil {
			out = append(out, bankInfo{Name: name, Error: err.Error(), Connectors: []connectorInfo{}})
			continue
		}
		info := bankInfo{
			Name: b.Name, Dir: b.Dir, Embedder: b.Embedder.String(), Endpoint: b.Embedder.Endpoint,
			Chunker: b.Chunker, Seeds: b.Search.Seeds, EdgeWeights: b.Search.EdgeWeights,
			Counts:     counts{Nodes: len(snap.state.Nodes), Edges: len(snap.state.Edges)},
			Connectors: []connectorInfo{},
		}
		for name, p := range b.Chunkers {
			if info.Budgets == nil {
				info.Budgets = map[string]int{}
			}
			info.Budgets[name] = p.Budget
		}
		perConnector, kinds := map[string]int{}, map[string]map[string]int{}
		for _, n := range snap.state.Nodes {
			info.Counts.Chunks += len(n.Chunks)
			if len(n.Chunks) > 0 {
				info.Counts.TextNodes++
			}
			perConnector[n.Connector]++
			if kinds[n.Connector] == nil {
				kinds[n.Connector] = map[string]int{}
			}
			kinds[n.Connector][n.Kind]++
		}
		st := semantic.Status(snap.state, b.Semantic)
		info.Layer = layerInfo{Entities: st.Counts.Entities, Topics: st.Counts.Topics, Mentions: st.Mentions, Read: st.Processed, Pending: st.Pending}
		info.Enrich, info.Compacted, info.IndexEvery = enrich.Status(b, snap.state), len(snap.state.Compacted), b.IndexEvery
		if emb, err := embedder.New(b.Embedder); err == nil {
			if v, err := index.OpenVectors(b.IndexDir(), emb.Key()); err == nil {
				info.Counts.Vectors = v.Len()
			}
		}
		for _, c := range b.Connectors {
			ci := connectorInfo{Name: c.Name, Command: c.Command, Nodes: perConnector[c.Name], Kinds: kinds[c.Name], Cursor: snap.state.Cursors[c.Name]}
			if run, ok := snap.state.Runs[c.Name]; ok {
				ci.LastRun, ci.RunErr = &run.At, run.Err
			}
			info.Connectors = append(info.Connectors, ci)
		}
		out = append(out, info)
	}
	reply(w, map[string]any{"banks": out})
}

type graphNode struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Label     string     `json:"label"`
	Degree    int        `json:"degree"`
	Chunks    int        `json:"chunks"`
	Connector string     `json:"connector,omitempty"`
	At        *time.Time `json:"at,omitempty"`
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// graph is the bank's nodes and edges, cut to `limit` nodes (DefaultLimit when
// absent, 0 for all of them). The cut keeps the nodes of the highest degree —
// the hubs are what gives a drawing its shape — and then adds every id given
// as `with` together with its direct neighbours, over the limit, so a search's
// hits and what surrounds them are drawn however small their degree. `kind`
// narrows the nodes to one kind before the cut. An edge is returned when both
// its ends are. The kind counts are the whole bank's, whatever the cut.
func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	_, snap, err := s.load(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	q := r.URL.Query()
	limit := DefaultLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 0 {
			fail(w, http.StatusBadRequest, errors.New("limit: a number, zero or more"))
			return
		}
	}
	kind := q.Get("kind")
	// hide are the kinds the page has switched off. They take none of the
	// limit's places: a kind of leaves, which the highest degrees never
	// reach, is drawn once the kinds above it are switched off.
	hide := map[string]bool{}
	for _, k := range q["hide"] {
		hide[k] = true
	}

	in := map[string]bool{}
	matching := 0
	// around draws one node's surroundings and nothing else: the node, the
	// nodes pointing at it and what those are tied to — a session with its
	// messages, their entities, rules and files.
	around := q.Get("around")
	if snap.state.Nodes[around] != nil {
		for part := range snap.parts(around) {
			in[part] = true
			for _, e := range snap.adjacent[part] {
				in[e.From], in[e.To] = true, true
			}
		}
		for id := range in {
			if hide[snap.state.Nodes[id].Kind] {
				delete(in, id)
			}
		}
		matching = len(in)
	} else {
		for _, id := range snap.byDegree {
			if k := snap.state.Nodes[id].Kind; (kind != "" && k != kind) || hide[k] {
				continue
			}
			matching++
			if limit == 0 || len(in) < limit {
				in[id] = true
			}
		}
	}
	for _, id := range q["with"] {
		if snap.state.Nodes[id] == nil {
			continue
		}
		in[id] = true
		for _, e := range snap.adjacent[id] {
			in[e.From], in[e.To] = true, true
		}
	}

	nodes := make([]graphNode, 0, len(in))
	for _, id := range snap.byDegree {
		if !in[id] {
			continue
		}
		n := snap.state.Nodes[id]
		gn := graphNode{ID: id, Kind: n.Kind, Label: label(n), Degree: len(snap.adjacent[id]), Chunks: len(n.Chunks), Connector: n.Connector}
		if !n.At.IsZero() {
			gn.At = &n.At
		}
		nodes = append(nodes, gn)
	}
	edges := []graphEdge{}
	for _, e := range snap.state.Edges {
		if in[e.From] && in[e.To] {
			edges = append(edges, graphEdge{From: e.From, To: e.To, Kind: e.Kind})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
	reply(w, map[string]any{
		"nodes": nodes, "edges": edges,
		"total_nodes": len(snap.state.Nodes), "total_edges": snap.edges,
		"truncated":  len(nodes) < matching,
		"node_kinds": snap.nodeKinds, "edge_kinds": snap.edgeKinds,
	})
}

// ListLimit is how many nodes a listing holds when none is asked for.
const ListLimit = 1000

type listed struct {
	ID     string     `json:"id"`
	Kind   string     `json:"kind"`
	Label  string     `json:"label"`
	Degree int        `json:"degree"`
	At     *time.Time `json:"at,omitempty"`
	// About is what tells a node with no text from the others of its kind: the
	// earliest text pointing at it, a session's first message, or its attrs.
	About string            `json:"about,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
	// Parts and Reach are the node's reach in numbers: how many nodes point at
	// it, and how many nodes of each kind those are tied to.
	Parts int            `json:"parts,omitempty"`
	Reach map[string]int `json:"reach,omitempty"`
}

// nodes lists the nodes of a kind, the latest first: the way to a branch or a
// session, which no search finds and the drawing does not name. A node with no
// text is as late as the latest of its parts, so a branch is as late as the
// last message of its sessions. `q` keeps the nodes whose id, label or attrs hold it.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	_, snap, err := s.load(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	q := r.URL.Query()
	kind, want := q.Get("kind"), strings.ToLower(strings.TrimSpace(q.Get("q")))
	limit := ListLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			fail(w, http.StatusBadRequest, errors.New("limit: a number above zero"))
			return
		}
	}
	out := []listed{}
	for id, n := range snap.state.Nodes {
		if n.Kind != kind {
			continue
		}
		if want != "" {
			held := strings.ToLower(id + "\n" + label(n))
			for _, v := range n.Attrs {
				held += "\n" + strings.ToLower(v)
			}
			if !strings.Contains(held, want) {
				continue
			}
		}
		l := listed{ID: id, Kind: n.Kind, Label: label(n), Degree: len(snap.adjacent[id])}
		at := n.At
		var first *index.Node
		for _, e := range snap.adjacent[id] {
			from := snap.state.Nodes[e.From]
			if e.To != id || e.From == id {
				continue
			}
			if from.At.After(at) {
				at = from.At
			}
			if len(from.Chunks) > 0 && (first == nil || from.At.Before(first.At)) {
				first = from
			}
		}
		if !at.IsZero() {
			l.At = &at
		}
		l.Attrs = n.Attrs
		if len(n.Chunks) == 0 {
			// A branch is as late as the last message of its sessions.
			for part := range snap.parts(id) {
				if p := snap.state.Nodes[part]; p.At.After(at) {
					at = p.At
				}
			}
			if !at.IsZero() {
				l.At = &at
			}
			parts, groups := snap.reach(id)
			l.Parts, l.Reach = parts, map[string]int{}
			for _, g := range groups {
				if g.Dir == "out" {
					l.Reach[g.NodeKind] += g.Total
				}
			}
			if first != nil {
				l.About = label(first)
			} else {
				keys := make([]string, 0, len(n.Attrs))
				for k := range n.Attrs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					l.About += k + "=" + n.Attrs[k] + " "
				}
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.At != nil && b.At != nil && !a.At.Equal(*b.At):
			return a.At.After(*b.At)
		case (a.At == nil) != (b.At == nil):
			return a.At != nil
		case a.Degree != b.Degree:
			return a.Degree > b.Degree
		}
		return a.ID < b.ID
	})
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	reply(w, map[string]any{"nodes": out, "total": total})
}

// ReachLimit is how many nodes a group of the reach lists; Total says how many
// there were.
const ReachLimit = 200

type reachGroup struct {
	Kind string `json:"kind"`
	// Dir is "out" for edges from the parts, "in" for edges to them.
	Dir      string `json:"dir"`
	NodeKind string `json:"node_kind"`
	Total    int    `json:"total"`
	// A neighbour's weight is how many of the parts are tied to it.
	Neighbours []neighbour `json:"neighbours"`
}

// reach is what a node with no text of its own is tied to through its parts.
// The parts are the nodes that point at it, and at those of them with no text
// in turn: the sessions on a branch and the messages in them. What the parts
// point at outside themselves is grouped by the edge's kind and the kind of
// the node, the most tied first. A session compacted onto its own node and one
// still holding its messages read the same.
func (snap *snapshot) reach(id string) (int, []reachGroup) {
	parts := snap.parts(id)
	tied := map[[3]string]map[string]float64{}
	// The node's own edges count with its parts': a session compacted onto its
	// node carries there what its messages did. Edges to the parts count as
	// well: the passages that state a rule are what it stands among.
	for part := range parts {
		for _, e := range snap.adjacent[part] {
			dir, other := "out", e.To
			if e.From != part {
				dir, other = "in", e.From
			}
			if parts[other] {
				continue
			}
			key := [3]string{dir, e.Kind, snap.state.Nodes[other].Kind}
			if tied[key] == nil {
				tied[key] = map[string]float64{}
			}
			tied[key][other]++
		}
	}
	return len(parts) - 1, groupsOf(snap, tied)
}

// parts are the node and, when it has no text, the nodes that point at it, and
// at those of them with no text in turn. A node with text is its only part:
// what points at a rule is the sessions that state it, whole.
func (snap *snapshot) parts(id string) map[string]bool {
	parts := map[string]bool{id: true}
	queue := []string{id}
	if len(snap.state.Nodes[id].Chunks) > 0 {
		queue = nil
	}
	for ; len(queue) > 0; queue = queue[1:] {
		for _, e := range snap.adjacent[queue[0]] {
			if e.To != queue[0] || parts[e.From] {
				continue
			}
			parts[e.From] = true
			if len(snap.state.Nodes[e.From].Chunks) == 0 {
				queue = append(queue, e.From)
			}
		}
	}
	return parts
}

func groupsOf(snap *snapshot, tied map[[3]string]map[string]float64) []reachGroup {
	groups := make([]reachGroup, 0, len(tied))
	for key, to := range tied {
		g := reachGroup{Dir: key[0], Kind: key[1], NodeKind: key[2], Total: len(to)}
		for other, n := range to {
			o := snap.state.Nodes[other]
			g.Neighbours = append(g.Neighbours, neighbour{ID: other, Kind: o.Kind, Label: label(o), Weight: n, Text: textOf(o)})
		}
		sort.Slice(g.Neighbours, func(i, j int) bool {
			a, b := g.Neighbours[i], g.Neighbours[j]
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
			return a.ID < b.ID
		})
		if len(g.Neighbours) > ReachLimit {
			g.Neighbours = g.Neighbours[:ReachLimit]
		}
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Dir != groups[j].Dir {
			return groups[i].Dir > groups[j].Dir // out first
		}
		if groups[i].Kind != groups[j].Kind {
			return groups[i].Kind < groups[j].Kind
		}
		return groups[i].NodeKind < groups[j].NodeKind
	})
	return groups
}

type nodeChunk struct {
	Prefix string `json:"prefix,omitempty"`
	Lines  []int  `json:"lines,omitempty"`
	Text   string `json:"text"`
}

type edgeGroup struct {
	Kind string `json:"kind"`
	// Dir is "out" for edges from the node, "in" for edges to it.
	Dir        string      `json:"dir"`
	Neighbours []neighbour `json:"neighbours"`
}

type neighbour struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Weight float64 `json:"weight,omitempty"`
	// Text is set in a reach: a rule's text, an entity's summary, the opening
	// of a document — what is read there without opening the node.
	Text string `json:"text,omitempty"`
}

// ReachText is how much of a node's text a reach carries.
const ReachText = 1200

func textOf(n *index.Node) string {
	t := n.Attrs["summary"]
	if len(n.Chunks) > 0 {
		t = n.Chunks[0].Text
	}
	if r := []rune(t); len(r) > ReachText {
		return string(r[:ReachText]) + "…"
	}
	return t
}

func (s *Server) node(w http.ResponseWriter, r *http.Request) {
	_, snap, err := s.load(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	id := r.URL.Query().Get("id")
	n := snap.state.Nodes[id]
	if n == nil {
		fail(w, http.StatusNotFound, errors.New("no node "+strconv.Quote(id)))
		return
	}
	chunks := make([]nodeChunk, len(n.Chunks))
	for i, c := range n.Chunks {
		chunks[i] = nodeChunk{Prefix: c.Prefix, Lines: lineRange(c), Text: c.Text}
	}
	groups := map[[2]string]*edgeGroup{}
	for _, e := range snap.adjacent[id] {
		dir, other := "out", e.To
		if e.From != id {
			dir, other = "in", e.From
		}
		g := groups[[2]string{dir, e.Kind}]
		if g == nil {
			g = &edgeGroup{Kind: e.Kind, Dir: dir}
			groups[[2]string{dir, e.Kind}] = g
		}
		o := snap.state.Nodes[other]
		g.Neighbours = append(g.Neighbours, neighbour{ID: other, Kind: o.Kind, Label: label(o), Weight: e.Weight})
	}
	edges := make([]edgeGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.Neighbours, func(i, j int) bool { return g.Neighbours[i].ID < g.Neighbours[j].ID })
		edges = append(edges, *g)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Dir != edges[j].Dir {
			return edges[i].Dir > edges[j].Dir // out first
		}
		return edges[i].Kind < edges[j].Kind
	})
	out := map[string]any{
		"id": n.ID, "kind": n.Kind, "label": label(n), "attrs": n.Attrs, "connector": n.Connector,
		"degree": len(snap.adjacent[id]), "chunks": chunks, "edges": edges,
	}
	if parts, groups := snap.reach(id); len(groups) > 0 {
		out["reach"] = map[string]any{"parts": parts, "groups": groups}
	}
	if !n.At.IsZero() {
		out["at"] = n.At
	}
	reply(w, out)
}

type hit struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"`
	Label   string            `json:"label"`
	Heading string            `json:"heading,omitempty"`
	Lines   []int             `json:"lines,omitempty"`
	Snippet string            `json:"snippet,omitempty"`
	Score   float64           `json:"score"`
	Mass    float64           `json:"mass"`
	Seed    bool              `json:"seed"`
	Also    [][]int           `json:"also,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	b, snap, err := s.load(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		fail(w, http.StatusBadRequest, errors.New("q: a query"))
		return
	}
	o := search.Options{K: 10, Kind: q.Get("kind"), NoGraph: truthy(q.Get("nograph"))}
	if v := q.Get("k"); v != "" {
		if o.K, err = strconv.Atoi(v); err != nil || o.K <= 0 {
			fail(w, http.StatusBadRequest, errors.New("k: a positive number"))
			return
		}
	}
	hits, warning, err := snap.corpus.Run(r.Context(), b, query, o)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	out := make([]hit, len(hits))
	for i, h := range hits {
		out[i] = hit{
			ID: h.Node.ID, Kind: h.Node.Kind, Label: label(h.Node), Heading: h.Chunk.Prefix, Lines: lineRange(h.Chunk),
			Snippet: search.Render{}.Snippet(h.Chunk.Text, query),
			Score:   h.Score, Mass: h.Mass, Seed: h.Seed, Attrs: h.Node.Attrs,
		}
		for _, c := range h.Also {
			out[i].Also = append(out[i].Also, lineRange(c))
		}
	}
	reply(w, map[string]any{"query": query, "hits": out, "warning": warning})
}

// Query is `muninn search` asked of a running server: the options and the
// rendering of the command, and its output as the answer's body. The command
// asks here first, so a query does not pay for loading the bank.
type Query struct {
	Text    string
	Options search.Options
	Render  search.Render
	JSON    bool
}

// Values is the query as the URL carries it, and ParseQuery reads it back.
func (q Query) Values() url.Values {
	v := url.Values{"q": {q.Text}, "k": {strconv.Itoa(q.Options.K)}, "chars": {strconv.Itoa(q.Render.Chars)}}
	for k, val := range q.Options.Filters {
		v.Add("filter", k+"="+val)
	}
	for _, a := range q.Render.Attrs {
		v.Add("attr", a)
	}
	set := func(key string, on bool) {
		if on {
			v.Set(key, "1")
		}
	}
	set("nograph", q.Options.NoGraph)
	set("full", q.Render.Full)
	set("json", q.JSON)
	if q.Options.Kind != "" {
		v.Set("kind", q.Options.Kind)
	}
	if q.Options.Owner != "" {
		v.Set("owner", q.Options.Owner)
	}
	if q.Options.From != "" {
		v.Set("from", q.Options.From)
	}
	if q.Options.HalfLife > 0 {
		v.Set("halflife", q.Options.HalfLife.String())
	}
	if !q.Options.Since.IsZero() {
		v.Set("since", q.Options.Since.Format(time.RFC3339Nano))
	}
	return v
}

func ParseQuery(v url.Values) (Query, error) {
	q := Query{Text: strings.TrimSpace(v.Get("q")), JSON: truthy(v.Get("json"))}
	if q.Text == "" && v.Get("from") == "" {
		return q, errors.New("q: a query")
	}
	q.Options = search.Options{From: v.Get("from"), Kind: v.Get("kind"), Owner: v.Get("owner"), NoGraph: truthy(v.Get("nograph")), Filters: map[string]string{}}
	q.Render = search.Render{Full: truthy(v.Get("full")), Attrs: v["attr"]}
	var err error
	if q.Options.K, err = strconv.Atoi(v.Get("k")); err != nil {
		return q, errors.New("k: a number")
	}
	if q.Render.Chars, err = strconv.Atoi(v.Get("chars")); err != nil {
		return q, errors.New("chars: a number")
	}
	for _, f := range v["filter"] {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			return q, errors.New("filter: key=value")
		}
		q.Options.Filters[k] = val
	}
	if hl := v.Get("halflife"); hl != "" {
		if q.Options.HalfLife, err = time.ParseDuration(hl); err != nil {
			return q, errors.New("halflife: a duration")
		}
	}
	if since := v.Get("since"); since != "" {
		if q.Options.Since, err = time.Parse(time.RFC3339Nano, since); err != nil {
			return q, errors.New("since: RFC 3339")
		}
	}
	return q, nil
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	b, snap, err := s.load(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	q, err := ParseQuery(r.URL.Query())
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	hits, warning, err := snap.corpus.Run(r.Context(), b, q.Text, q.Options)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	q.Render.Warning = warning
	if q.JSON {
		w.Header().Set("Content-Type", "application/json")
		q.Render.JSON(w, q.Text, hits)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	q.Render.Text(w, q.Text, hits)
}

// Warm loads every bank, so the first query of each is as quick as the rest.
func (s *Server) Warm(log io.Writer) {
	names, err := bank.List()
	if err != nil {
		fmt.Fprintln(log, "muninn:", err)
		return
	}
	for _, name := range names {
		start := time.Now()
		if _, snap, err := s.load(name); err != nil {
			fmt.Fprintf(log, "muninn: bank %s: %v\n", name, err)
		} else {
			fmt.Fprintf(log, "bank %s loaded in %s: %d nodes\n", name, time.Since(start).Round(time.Millisecond), len(snap.state.Nodes))
		}
	}
}

func label(n *index.Node) string { return n.Label() }

func lineRange(c index.Chunk) []int {
	if c.Line <= 0 {
		return nil
	}
	return []int{c.Line, max(c.Line, c.EndLine)}
}

// usageInfo is what the paid calls came to: for every bank, for this one, for
// this one today, and this one's split by purpose and by model.
type usageInfo struct {
	All       usage.Line   `json:"all"`
	Bank      usage.Line   `json:"bank"`
	Today     usage.Line   `json:"today"`
	ByPurpose []usage.Line `json:"by_purpose"`
	ByModel   []usage.Line `json:"by_model"`
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	info, err := usageOf(r.PathValue("bank"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, info)
}

func usageOf(name string) (usageInfo, error) {
	var info usageInfo
	home, err := bank.Home()
	if err != nil {
		return info, err
	}
	recs, err := usage.Read(home)
	if err != nil {
		return info, err
	}
	prices, err := usage.Prices(home)
	if err != nil {
		return info, err
	}
	own := usage.Filter(recs, name, time.Time{})
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	_, info.All, _ = usage.Summarize(recs, prices, "bank")
	_, info.Today, _ = usage.Summarize(usage.Filter(own, name, midnight), prices, "bank")
	info.ByPurpose, info.Bank, _ = usage.Summarize(own, prices, "purpose")
	info.ByModel, _, _ = usage.Summarize(own, prices, "model")
	return info, nil
}

func truthy(v string) bool { return v == "1" || v == "true" || v == "on" }

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
