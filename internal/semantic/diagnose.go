package semantic

import (
	"sort"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
)

// Diagnosis is the whole bank looked at once: what `graph stats` prints. The
// gate never computes it; it measures regions.
type Diagnosis struct {
	Nodes      map[string]int           `json:"nodes"`
	Edges      map[string]int           `json:"edges"`
	Components Components               `json:"components"`
	Degrees    map[string]DegreeSummary `json:"degrees"`
	Full       GraphMetrics             `json:"full_graph"`
	Semantic   GraphMetrics             `json:"semantic_graph"`
}

type Components struct {
	Count        int     `json:"count"`
	Largest      int     `json:"largest"`
	LargestShare float64 `json:"largest_share"`
	Isolated     int     `json:"isolated"`
}

// DegreeSummary is one node kind's degrees in the full graph, with its hubs.
type DegreeSummary struct {
	Nodes  int     `json:"nodes"`
	Mean   float64 `json:"mean"`
	Median int     `json:"median"`
	P90    int     `json:"p90"`
	Max    int     `json:"max"`
	Hubs   []Hub   `json:"hubs"`
}

type Hub struct {
	ID     string `json:"id"`
	Kind   string `json:"kind,omitempty"`
	Degree int    `json:"degree"`
	// Reach is set by the fractal heuristic: the sum of the neighbours' degrees,
	// an upper bound on how many nodes sit within two hops.
	Reach int `json:"reach,omitempty"`
}

// GraphMetrics are the two metrics over a whole graph, with the estimators'
// parameters taken from the bank's checks.
type GraphMetrics struct {
	Nodes     int         `json:"nodes"`
	Edges     int         `json:"edges"`
	ScaleFree Measurement `json:"scale_free"`
	Fractal   Measurement `json:"fractal"`
}

func Diagnose(state *index.State, cfg bank.Semantic, top int) Diagnosis {
	d := Diagnosis{Nodes: map[string]int{}, Edges: map[string]int{}, Degrees: map[string]DegreeSummary{}}
	for _, n := range state.Nodes {
		d.Nodes[n.Kind]++
	}
	for _, e := range state.Edges {
		d.Edges[e.Kind]++
	}
	full := WholeView(state, false)
	d.Components = components(full)
	byKind := map[string][]string{}
	for _, id := range full.Nodes {
		k := state.Nodes[id].Kind
		byKind[k] = append(byKind[k], id)
	}
	for kind, ids := range byKind {
		d.Degrees[kind] = summarize(state, full, ids, top)
	}
	d.Full = measureWhole(full, cfg)
	d.Semantic = measureWhole(WholeView(state, true), cfg)
	return d
}

func measureWhole(v *GraphView, cfg bank.Semantic) GraphMetrics {
	checks := cfg.Resolved().Checks
	sf, fr := checks["scale_free"].Params, checks["fractal"].Params
	m := GraphMetrics{Nodes: len(v.Nodes)}
	for _, id := range v.Nodes {
		m.Edges += len(v.Adj[id])
	}
	m.Edges /= 2
	m.ScaleFree = ScaleFree(v, int(sf["min_nodes"]), int(sf["min_tail"]), 0)
	m.Fractal = Fractal(v, int(fr["min_nodes"]), int(fr["centres"]), int(fr["radius"]))
	return m
}

func components(v *GraphView) Components {
	var c Components
	seen := map[string]bool{}
	for _, start := range v.Nodes {
		if seen[start] {
			continue
		}
		seen[start] = true
		size, stack := 0, []string{start}
		for len(stack) > 0 {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			size++
			for _, w := range v.Adj[u] {
				if !seen[w] {
					seen[w] = true
					stack = append(stack, w)
				}
			}
		}
		c.Count++
		c.Largest = max(c.Largest, size)
		if size == 1 {
			c.Isolated++
		}
	}
	if len(v.Nodes) > 0 {
		c.LargestShare = float64(c.Largest) / float64(len(v.Nodes))
	}
	return c
}

func summarize(state *index.State, v *GraphView, ids []string, top int) DegreeSummary {
	degrees := make([]int, len(ids))
	total := 0
	for i, id := range ids {
		degrees[i] = v.Degree[id]
		total += degrees[i]
	}
	sort.Ints(degrees)
	n := len(ids)
	return DegreeSummary{
		Nodes: n, Mean: float64(total) / float64(n), Median: degrees[n/2], P90: degrees[min(n-1, n*9/10)], Max: degrees[n-1],
		Hubs: TopHubs(state, v, ids, top),
	}
}

// TopHubs are the nodes of highest degree among ids, ties by id.
func TopHubs(state *index.State, v *GraphView, ids []string, top int) []Hub {
	hubs := make([]Hub, 0, len(ids))
	for _, id := range ids {
		if v.Degree[id] > 0 {
			hubs = append(hubs, Hub{ID: id, Kind: state.Nodes[id].Kind, Degree: v.Degree[id]})
		}
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].Degree != hubs[j].Degree {
			return hubs[i].Degree > hubs[j].Degree
		}
		return hubs[i].ID < hubs[j].ID
	})
	return hubs[:min(top, len(hubs))]
}

// Shortcuts are the nodes that put the most of the graph within two hops of
// themselves, by the sum of their neighbours' degrees. It is a heuristic for
// which nodes flatten the ball growth, not a measurement of what removing one
// would do: that would take a fractal estimate per candidate.
func Shortcuts(state *index.State, v *GraphView, top int) []Hub {
	hubs := make([]Hub, 0, len(v.Nodes))
	for _, id := range v.Nodes {
		reach := 0
		for _, w := range v.Adj[id] {
			reach += v.Degree[w]
		}
		if reach > 0 {
			hubs = append(hubs, Hub{ID: id, Kind: state.Nodes[id].Kind, Degree: v.Degree[id], Reach: reach})
		}
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].Reach != hubs[j].Reach {
			return hubs[i].Reach > hubs[j].Reach
		}
		return hubs[i].ID < hubs[j].ID
	})
	return hubs[:min(top, len(hubs))]
}

// Finding is one metric of one graph set against the bank's band.
type Finding struct {
	Graph   string      `json:"graph"`
	Check   string      `json:"check"`
	Verdict string      `json:"verdict"` // in_band, miss, not_applicable
	Measure Measurement `json:"measurement"`
	Loss    float64     `json:"loss"`
	Note    string      `json:"note,omitempty"`
	Blame   []Hub       `json:"blame,omitempty"`
}

// CheckBank sets the whole-graph metrics against the bank's bands and names,
// for each miss, the nodes most behind it. It reads and never writes.
func CheckBank(state *index.State, cfg bank.Semantic, top int) []Finding {
	checks := cfg.Resolved().Checks
	sf, fr := scaleFreeCheck{checks["scale_free"].Params}, checks["fractal"].Params
	var out []Finding
	for _, g := range []struct {
		name string
		view *GraphView
	}{{"semantic", WholeView(state, true)}, {"full", WholeView(state, false)}} {
		m := measureWhole(g.view, cfg)

		f := Finding{Graph: g.name, Check: "scale_free", Measure: m.ScaleFree, Verdict: "not_applicable"}
		if m.ScaleFree.Status == StatusOK {
			f.Verdict = "in_band"
			if f.Loss = sf.loss(m.ScaleFree); f.Loss > 0 {
				f.Verdict = "miss"
				f.Note = "highest degrees"
				f.Blame = TopHubs(state, g.view, g.view.Nodes, top)
			}
		}
		out = append(out, f)

		f = Finding{Graph: g.name, Check: "fractal", Measure: m.Fractal, Verdict: "not_applicable"}
		switch {
		case m.Fractal.Status == StatusOK:
			f.Verdict = "in_band"
			f.Loss = band(m.Fractal.Value, fr["low"], fr["high"])
			if m.Fractal.Value > fr["high"] {
				f.Verdict, f.Note, f.Blame = "miss", "largest two-hop reach, by the sum of the neighbours' degrees", Shortcuts(state, g.view, top)
			} else if f.Loss > 0 {
				f.Verdict, f.Note = "miss", "below the band the graph is stringy — chains and trees — and no node is behind that"
			}
		case m.Fractal.Status == StatusSaturated:
			f.Verdict, f.Note, f.Blame = "miss", "saturated within three hops; largest two-hop reach, by the sum of the neighbours' degrees", Shortcuts(state, g.view, top)
		}
		out = append(out, f)
	}
	return out
}

// LayerStatus is what `bank show` says of the layer.
type LayerStatus struct {
	Counts             Counts
	Mentions           int
	Processed, Pending int
}

// Status counts the layer and how much of the bank the stage has read with
// the bank's extractor as it is configured now.
func Status(state *index.State, cfg bank.Semantic) LayerStatus {
	layer := NewLayer(state)
	layer.countTexts(func(n *index.Node) bool { return Reads(cfg, n) })
	st := LayerStatus{Counts: layer.Counts()}
	for _, e := range state.Edges {
		if e.Connector == index.SemanticConnector && e.Kind == EdgeMentions {
			st.Mentions++
		}
	}
	id := ExtractorID(cfg)
	if id == "" {
		// With the method off nothing is pending; what was read stays read.
		for id := range state.Semantic {
			if IsText(state.Nodes[id]) {
				st.Processed++
			}
		}
		return st
	}
	st.Pending = len(Pending(state, cfg, id))
	st.Processed = st.Counts.Texts - st.Pending
	return st
}
