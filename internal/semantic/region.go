package semantic

import (
	"sort"

	"github.com/kgatilin/muninn/internal/index"
)

// GraphView is what a metric measures: a fixed set of nodes, their full
// degrees, and the graph induced on them. A region of the layer is one; the
// whole bank is another, which is how `graph stats` uses the same metrics.
type GraphView struct {
	// Nodes are the live nodes of the view, sorted. Anchors are the ones a
	// sample of centres may be drawn from; for a region they are the nodes live
	// both before and after the patch, so both measurements walk from the same
	// places.
	Nodes   []string
	Anchors []string
	// Degree is the node's degree in the whole graph, not in the induced one:
	// a region's edge cuts through its boundary nodes' neighbourhoods, and the
	// induced degree would make every one of them look like a leaf.
	Degree map[string]int
	Adj    map[string][]string
}

// Region is the fixed domain one patch is measured over.
type Region struct {
	Nodes []string
	// Truncated says the node cap cut a shell short; Boundary is how many
	// neighbours of the domain were left outside it.
	Truncated bool
	Boundary  int
}

// neighbors reads the adjacency as it is now (after) or as it was when the
// transaction began.
func (l *Layer) neighbors(id string, before bool) []string {
	out := make([]string, 0, len(l.adj[id]))
	for v, c := range l.adj[id] {
		if !before || l.tx == nil || c-l.tx.delta[id][v] > 0 {
			out = append(out, v)
		}
	}
	if before && l.tx != nil {
		for v, d := range l.tx.delta[id] {
			if d < 0 && l.adj[id][v] == 0 {
				out = append(out, v)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (l *Layer) exists(id string, before bool) bool {
	now := l.state.Nodes[id] != nil
	if !before || l.tx == nil {
		return now
	}
	return (now && !l.tx.added[id]) || l.tx.removed[id]
}

// region grows the patch's footprint by whole shells, hops times, over the
// union of the graph before the patch and after it: one domain for both
// measurements, so an edge the patch removed cannot take its old
// neighbourhood out of the baseline. A shell that would pass the cap is cut in
// id order and the region says so.
func (l *Layer) region(footprint []string, hops, limit int) Region {
	in := map[string]bool{}
	var r Region
	shell := footprint
	for h := 0; ; h++ {
		sort.Strings(shell)
		for _, id := range shell {
			if len(r.Nodes) >= limit {
				r.Truncated = true
				break
			}
			if !in[id] {
				in[id] = true
				r.Nodes = append(r.Nodes, id)
			}
		}
		if h == hops || r.Truncated {
			break
		}
		next := map[string]bool{}
		for _, id := range shell {
			if !in[id] {
				continue
			}
			for _, side := range []bool{true, false} {
				for _, v := range l.neighbors(id, side) {
					if !in[v] {
						next[v] = true
					}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		shell = shell[:0:0]
		for v := range next {
			shell = append(shell, v)
		}
	}
	sort.Strings(r.Nodes)
	outside := map[string]bool{}
	for _, id := range r.Nodes {
		for _, v := range l.neighbors(id, false) {
			if !in[v] {
				outside[v] = true
			}
		}
	}
	r.Boundary = len(outside)
	return r
}

// view is the region as it is now, or as it was before the open transaction.
func (l *Layer) view(r Region, before bool) *GraphView {
	v := &GraphView{Degree: map[string]int{}, Adj: map[string][]string{}}
	in := map[string]bool{}
	for _, id := range r.Nodes {
		if l.exists(id, before) {
			in[id] = true
			v.Nodes = append(v.Nodes, id)
			if l.exists(id, !before) {
				v.Anchors = append(v.Anchors, id)
			}
		}
	}
	for _, id := range v.Nodes {
		nbrs := l.neighbors(id, before)
		v.Degree[id] = len(nbrs)
		for _, w := range nbrs {
			if in[w] {
				v.Adj[id] = append(v.Adj[id], w)
			}
		}
	}
	return v
}

// WholeView is every node with its edges as one view. With semanticOnly it is
// the graph the gate measures regions of — text nodes, entities, topics and
// the layer's own edges; without, it is the bank's full graph.
func WholeView(state *index.State, semanticOnly bool) *GraphView {
	sets := map[string]map[string]bool{}
	for _, e := range state.Edges {
		if e.From == e.To || state.Nodes[e.From] == nil || state.Nodes[e.To] == nil {
			continue
		}
		if semanticOnly && e.Connector != index.SemanticConnector {
			continue
		}
		for _, p := range [2][2]string{{e.From, e.To}, {e.To, e.From}} {
			if sets[p[0]] == nil {
				sets[p[0]] = map[string]bool{}
			}
			sets[p[0]][p[1]] = true
		}
	}
	v := &GraphView{Degree: map[string]int{}, Adj: map[string][]string{}}
	for id, n := range state.Nodes {
		if semanticOnly && !IsText(n) && !IsSemantic(n) {
			continue
		}
		v.Nodes = append(v.Nodes, id)
		v.Degree[id] = len(sets[id])
		for w := range sets[id] {
			v.Adj[id] = append(v.Adj[id], w)
		}
		sort.Strings(v.Adj[id])
	}
	sort.Strings(v.Nodes)
	v.Anchors = v.Nodes
	return v
}
