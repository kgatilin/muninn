package search

import (
	"math"
	"sort"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
)

const (
	pprAlpha   = 0.15
	pprEpsilon = 1e-4
	// hubDamping divides an edge's weight by (deg u × deg v)^tau, so a node
	// everything hangs off passes less mass along each of its edges.
	hubDamping = 0.25
)

type link struct {
	to string
	w  float64
}

// walkGraph is the bank's edges as the walk sees them: undirected, parallel
// edges summed, weighted per kind, hub-damped. Adjacency is sorted so the
// floating-point sums come out the same on every run.
type walkGraph struct {
	adj map[string][]link
	deg map[string]float64
}

// newWalkGraph takes an edge's weight as the kind's configured weight (1 when
// the kind is not configured) times the edge's own Weight when that is
// positive. A kind configured to 0 is left out of the walk, as are self-loops
// and edges to a node the bank does not hold.
//
// The bank's two other cuts apply here and leave the stored graph alone: an
// edge touching a muted node is left out, and so is an edge of a capped kind
// at a node holding more edges of that kind than the cap, counted as stored.
func newWalkGraph(state *index.State, cfg bank.Search) *walkGraph {
	kindWeights := cfg.EdgeWeights
	muted := make(map[string]bool, len(cfg.Mute))
	for _, id := range cfg.Mute {
		muted[id] = true
	}
	stored := func(e *index.Edge) bool {
		return e.From != e.To && state.Nodes[e.From] != nil && state.Nodes[e.To] != nil
	}
	type kindAt struct{ kind, node string }
	count := map[kindAt]int{}
	if len(cfg.DegreeCap) > 0 {
		for _, e := range state.Edges {
			if _, capped := cfg.DegreeCap[e.Kind]; capped && stored(e) {
				count[kindAt{e.Kind, e.From}]++
				count[kindAt{e.Kind, e.To}]++
			}
		}
	}
	und := map[string]map[string]float64{}
	for _, e := range state.Edges {
		if !stored(e) || muted[e.From] || muted[e.To] {
			continue
		}
		if n, capped := cfg.DegreeCap[e.Kind]; capped && (count[kindAt{e.Kind, e.From}] > n || count[kindAt{e.Kind, e.To}] > n) {
			continue
		}
		w := 1.0
		if kw, ok := kindWeights[e.Kind]; ok {
			w = kw
		}
		if e.Weight > 0 {
			w *= e.Weight
		}
		if w <= 0 {
			continue
		}
		for _, p := range [2][2]string{{e.From, e.To}, {e.To, e.From}} {
			if und[p[0]] == nil {
				und[p[0]] = map[string]float64{}
			}
			und[p[0]][p[1]] += w
		}
	}
	g := &walkGraph{adj: make(map[string][]link, len(und)), deg: make(map[string]float64, len(und))}
	for u, nbrs := range und {
		ls := make([]link, 0, len(nbrs))
		for v, w := range nbrs {
			ls = append(ls, link{v, w})
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i].to < ls[j].to })
		g.adj[u] = ls
	}
	g.degrees()
	// Every edge is damped against the degrees as they stood before damping.
	orig := g.deg
	g.deg = make(map[string]float64, len(orig))
	for u, ls := range g.adj {
		for i := range ls {
			ls[i].w /= math.Pow(orig[u]*orig[ls[i].to], hubDamping)
		}
	}
	g.degrees()
	return g
}

func (g *walkGraph) degrees() {
	for u, ls := range g.adj {
		var d float64
		for _, l := range ls {
			d += l.w
		}
		g.deg[u] = d
	}
}

// ppr is approximate personalized PageRank by ACL residual push over the lazy
// walk. seeds is the personalization vector and must sum to 1; order fixes the
// sequence the seeds enter the queue. A node with no edges keeps all of its
// seed mass: a walk that starts there never leaves.
func (g *walkGraph) ppr(seeds map[string]float64, order []string) map[string]float64 {
	p := map[string]float64{}
	r := make(map[string]float64, len(seeds))
	var queue []string
	queued := map[string]bool{}
	enqueue := func(u string) {
		if !queued[u] && r[u] > pprEpsilon*g.deg[u] {
			queue = append(queue, u)
			queued[u] = true
		}
	}
	for _, id := range order {
		if g.deg[id] == 0 {
			p[id] += seeds[id]
			continue
		}
		r[id] += seeds[id]
		enqueue(id)
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		queued[u] = false
		ru, deg := r[u], g.deg[u]
		p[u] += pprAlpha * ru
		// Half of what does not teleport stays, half spreads by edge weight.
		spread := (1 - pprAlpha) * ru / 2
		r[u] = spread
		for _, l := range g.adj[u] {
			r[l.to] += spread * l.w / deg
			enqueue(l.to)
		}
		enqueue(u)
	}
	return p
}
