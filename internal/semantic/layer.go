// Package semantic builds the edges no source states: entities, topics, and
// the mentions that tie text nodes to them. Every write to the layer is a
// patch that passes a gate — the affected region is measured, the patch
// applied, the region measured again, and the patch undone when a check says
// it made the structure worse.
//
// The layer lives in the bank's own graph under index.SemanticConnector, so
// search, the walk and the UI see one graph.
package semantic

import (
	"sort"

	"github.com/kgatilin/muninn/internal/index"
)

const (
	KindEntity = "entity"
	// KindTopic is an intermediate node: it stands between an entity and some
	// of the text nodes that mentioned it.
	KindTopic = "topic"

	EdgeMentions = "mentions" // text node → entity or topic
	EdgePartOf   = "part_of"  // topic → entity
)

// Counts are the global live counters the budget and singleton checks read.
// Singletons are entities with exactly one neighbour.
type Counts struct {
	Texts, Entities, Topics, Edges, Singletons int
}

// Layer is the semantic graph over a State: text nodes, entity and topic
// nodes, and the edges the semantic connector owns. Structural edges are left
// out on purpose: they are facts their connectors own, nothing here may
// rewrite them, and a gate that measured them would hold a patch responsible
// for a shape it cannot change.
//
// All writes go through the four primitives at the bottom of this file, which
// keep the adjacency and the counters in step with the State and record their
// own inverses.
type Layer struct {
	state  *index.State
	adj    map[string]map[string]int // neighbour → parallel edges between the two
	counts Counts
	tx     *tx
}

// tx is one patch in flight: the inverses of what it did, and enough of the
// difference to read the graph as it was before.
type tx struct {
	undo    []func()
	delta   map[string]map[string]int // net change of adj, both directions
	added   map[string]bool
	removed map[string]bool
	before  Counts
}

func IsSemantic(n *index.Node) bool { return n != nil && n.Connector == index.SemanticConnector }

// IsText says whether the stage reads the node: it has text and the layer did
// not make it.
func IsText(n *index.Node) bool { return n != nil && !IsSemantic(n) && len(n.Chunks) > 0 }

func NewLayer(state *index.State) *Layer {
	l := &Layer{state: state, adj: map[string]map[string]int{}}
	for _, n := range state.Nodes {
		switch {
		case IsText(n):
			l.counts.Texts++
		case IsSemantic(n) && n.Kind == KindTopic:
			l.counts.Topics++
		case IsSemantic(n):
			l.counts.Entities++
		}
	}
	for _, e := range state.Edges {
		if e.Connector == index.SemanticConnector && state.Nodes[e.From] != nil && state.Nodes[e.To] != nil && e.From != e.To {
			l.link(e.From, e.To, 1)
		}
	}
	return l
}

// countTexts narrows the text-node count the budget reads to the nodes the
// stage reads: a kind left out of semantic.kinds earns no entities.
func (l *Layer) countTexts(reads func(*index.Node) bool) {
	l.counts.Texts = 0
	for _, n := range l.state.Nodes {
		if reads(n) {
			l.counts.Texts++
		}
	}
}

func (l *Layer) State() *index.State { return l.state }
func (l *Layer) Counts() Counts      { return l.counts }

func (l *Layer) Node(id string) *index.Node { return l.state.Nodes[id] }

// Degree is the number of distinct neighbours in the semantic graph.
func (l *Layer) Degree(id string) int { return len(l.adj[id]) }

func (l *Layer) Neighbors(id string) []string {
	out := make([]string, 0, len(l.adj[id]))
	for v := range l.adj[id] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (l *Layer) isEntity(id string) bool {
	n := l.state.Nodes[id]
	return IsSemantic(n) && n.Kind != KindTopic
}

// link changes the number of parallel edges between u and v by d.
func (l *Layer) link(u, v string, d int) {
	for _, p := range [2][2]string{{u, v}, {v, u}} {
		a, b := p[0], p[1]
		was := len(l.adj[a])
		if l.adj[a] == nil {
			l.adj[a] = map[string]int{}
		}
		if l.adj[a][b] += d; l.adj[a][b] == 0 {
			delete(l.adj[a], b)
			if len(l.adj[a]) == 0 {
				delete(l.adj, a)
			}
		}
		if l.isEntity(a) {
			l.counts.Singletons += b2i(len(l.adj[a]) == 1) - b2i(was == 1)
		}
		if l.tx != nil {
			if l.tx.delta[a] == nil {
				l.tx.delta[a] = map[string]int{}
			}
			l.tx.delta[a][b] += d
		}
	}
	l.counts.Edges += d
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (l *Layer) begin() {
	l.tx = &tx{delta: map[string]map[string]int{}, added: map[string]bool{}, removed: map[string]bool{}, before: l.counts}
}

// rollback runs the inverses, newest first. The transaction stays open while
// they run so that a caller may still read the difference; end closes it.
func (l *Layer) rollback() {
	t := l.tx
	for i := len(t.undo) - 1; i >= 0; i-- {
		t.undo[i]()
	}
	t.undo = nil
}

func (l *Layer) end() { l.tx = nil }

func (l *Layer) record(inverse func()) {
	if l.tx != nil {
		l.tx.undo = append(l.tx.undo, inverse)
	}
}

// putNode sets a semantic node, new or replacing. A node is never changed in
// place: the inverse puts the old pointer back, which is what makes undo exact.
func (l *Layer) putNode(n *index.Node) {
	old := l.state.Nodes[n.ID]
	if old == nil {
		l.count(n, 1)
		l.mark(n.ID, true)
	}
	l.state.Nodes[n.ID] = n
	l.record(func() {
		if old == nil {
			delete(l.state.Nodes, n.ID)
			l.count(n, -1)
			l.mark(n.ID, false)
		} else {
			l.state.Nodes[n.ID] = old
		}
	})
}

// dropNode removes a semantic node whose edges are already gone.
func (l *Layer) dropNode(id string) {
	old := l.state.Nodes[id]
	if old == nil {
		return
	}
	delete(l.state.Nodes, id)
	l.count(old, -1)
	l.mark(id, false)
	l.record(func() {
		l.state.Nodes[id] = old
		l.count(old, 1)
		l.mark(id, true)
	})
}

func (l *Layer) count(n *index.Node, d int) {
	if n.Kind == KindTopic {
		l.counts.Topics += d
	} else {
		l.counts.Entities += d
	}
}

// mark keeps the transaction's account of which nodes it brought in and which
// it took out; a node added and removed again is in neither.
func (l *Layer) mark(id string, added bool) {
	if l.tx == nil {
		return
	}
	mine, other := l.tx.removed, l.tx.added
	if added {
		mine, other = other, mine
	}
	if other[id] {
		delete(other, id)
	} else {
		mine[id] = true
	}
}

func (l *Layer) putEdge(e *index.Edge) {
	key := e.Key()
	old := l.state.Edges[key]
	l.state.Edges[key] = e
	if old == nil {
		l.link(e.From, e.To, 1)
	}
	l.record(func() {
		if old == nil {
			delete(l.state.Edges, key)
			l.link(e.From, e.To, -1)
		} else {
			l.state.Edges[key] = old
		}
	})
}

func (l *Layer) dropEdge(key string) {
	old := l.state.Edges[key]
	if old == nil {
		return
	}
	delete(l.state.Edges, key)
	l.link(old.From, old.To, -1)
	l.record(func() {
		l.state.Edges[key] = old
		l.link(old.From, old.To, 1)
	})
}

// edgesOf are the semantic edges touching a node, in key order. It scans the
// neighbours' possible keys and not the whole edge map.
func (l *Layer) edgesOf(id string) []*index.Edge {
	var out []*index.Edge
	for _, v := range l.Neighbors(id) {
		for _, kind := range []string{EdgeMentions, EdgePartOf} {
			for _, e := range []index.Edge{{From: id, To: v, Kind: kind}, {From: v, To: id, Kind: kind}} {
				if got := l.state.Edges[e.Key()]; got != nil && got.Connector == index.SemanticConnector {
					out = append(out, got)
				}
			}
		}
	}
	return out
}

// DropEdgesOf removes a node's semantic edges outside the gate: a text node
// that changed is about to be read again, and what it said before is no
// longer a fact.
func (l *Layer) DropEdgesOf(id string) {
	for _, e := range l.edgesOf(id) {
		l.dropEdge(e.Key())
	}
}

// Sweep removes topics that nothing is attached to any more and entities that
// fewer than min text nodes mention, directly or under one of their topics,
// and returns how many went. A rejected patch, a deleted document or a merge
// may leave an entity with fewer mentions than it was minted for, and one that
// ties too few text nodes together is a leaf. A topic that lost its last
// mention goes first, which may leave its entity bare.
func (l *Layer) Sweep(min int) int {
	removed := 0
	for {
		var bare []string
		for id, n := range l.state.Nodes {
			if !IsSemantic(n) {
				continue
			}
			if n.Kind == KindTopic {
				if l.texts(id) == 0 {
					bare = append(bare, id)
				}
				continue
			}
			held := l.texts(id)
			for v := range l.adj[id] {
				if IsSemantic(l.state.Nodes[v]) {
					held += l.texts(v)
				}
			}
			if held < max(min, 1) {
				bare = append(bare, id)
			}
		}
		if len(bare) == 0 {
			return removed
		}
		sort.Strings(bare)
		for _, id := range bare {
			l.DropEdgesOf(id)
			l.dropNode(id)
			removed++
		}
	}
}

// texts is how many text nodes are tied to the node.
func (l *Layer) texts(id string) int {
	n := 0
	for v := range l.adj[id] {
		if !IsSemantic(l.state.Nodes[v]) {
			n++
		}
	}
	return n
}

// Reset drops the whole layer and the record of what the stage has read.
func Reset(state *index.State) (nodes, edges int) {
	for id, n := range state.Nodes {
		if IsSemantic(n) {
			delete(state.Nodes, id)
			nodes++
		}
	}
	for k, e := range state.Edges {
		if e.Connector == index.SemanticConnector {
			delete(state.Edges, k)
			edges++
		}
	}
	state.Semantic = map[string]string{}
	state.Named = map[string]string{}
	return nodes, edges
}
