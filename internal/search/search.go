// Package search ranks a query against a bank: dense and lexical retrieval over
// chunks, fused by reciprocal rank, reduced to nodes by their best chunk; the
// best of those seed a personalized PageRank over the bank's edges, and the
// nodes are ranked by the mass it leaves on them.
package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/usage"
)

type Options struct {
	// K is how many hits a class of nodes may take of the answer.
	K       int
	Filters map[string]string // attr = value, all must hold
	Kind    string
	// Owner keeps the nodes whose connector's name starts with it: what one
	// stage of the engine made, whatever kinds it made.
	Owner string
	Since time.Time
	// HalfLife, when set, ranks by age too: a dated node's score and mass halve
	// for every HalfLife it is old. For what is rewritten as it changes — rules
	// — the later statement is the likelier to hold.
	HalfLife time.Duration
	// NoGraph skips the walk: the ranking is the fused text score alone.
	NoGraph bool
	// From, a node's id, asks what stands around that node and not what matches
	// a text: the walk starts from it alone. A branch so answers with its
	// ticket's design, the rules its sessions stated, the files they edited.
	From string
}

// Hit is a node, addressed through its best chunk. A node the walk reached
// that matched no text has Score 0 and its first chunk; a structural node has
// no chunk at all.
type Hit struct {
	Node  *index.Node
	Chunk index.Chunk
	// Score is the fused text score, Mass the PPR mass, Seed whether the node
	// personalized the walk.
	Score float64
	Mass  float64
	Seed  bool
	// Also are the node's other chunks that ranked, best first.
	Also []index.Chunk
	// Class is the bank's class of the node, "" for a structural one; the hits
	// of a class stand together. Sources is how many nodes state an item.
	Class   string
	Sources int
}

const (
	// channelDepth is how far down each channel's ranking the fusion reads.
	channelDepth = 200
	rrfK         = 60
	maxAlso      = 2
	// weak is the share of the best score, or of its class's best mass, under
	// which a hit is left out.
	weak = 1.0 / 3
	// reach is the same share for a walk from one node, where a hop away holds
	// several times less than the hop before it.
	reach = 0.05
)

// Corpus is what a query reads of a bank: the committed graph, its lexical
// index and the vectors under the bank's embedder. Load reads it from disk; a
// process that answers many queries keeps it for as long as Stamp holds.
type Corpus struct {
	State   *index.State
	Lexical *index.Lexical // nil for a bank indexed before there was one
	// Vectors is nil when they could not be opened, and VectorsErr says why;
	// the ranking is then lexical only.
	Vectors    *index.Vectors
	VectorsErr error
}

// Stamp changes when an `index` run commits: the mtime of the graph's file.
func Stamp(b *bank.Bank) time.Time {
	fi, err := os.Stat(filepath.Join(b.IndexDir(), "state.gob"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func Load(b *bank.Bank) (*Corpus, error) {
	dir := b.IndexDir()
	state, err := index.LoadState(dir)
	if err != nil {
		return nil, err
	}
	c := &Corpus{State: state}
	if c.Lexical, err = index.LoadLexical(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if emb, err := embed.New(b.Embedder); err != nil {
		c.VectorsErr = err
	} else if c.Vectors, err = index.OpenVectors(dir, emb.Key()); err != nil {
		c.Vectors, c.VectorsErr = nil, err
	}
	return c, nil
}

// Run loads the bank and answers one query.
func Run(ctx context.Context, b *bank.Bank, query string, o Options) ([]Hit, string, error) {
	c, err := Load(b)
	if err != nil {
		return nil, "", err
	}
	return c.Run(ctx, b, query, o)
}

// Run returns the hits and, when the embedder could not be reached, a warning:
// the ranking is then lexical only.
func (c *Corpus) Run(ctx context.Context, b *bank.Bank, query string, o Options) (hits []Hit, warning string, err error) {
	state := c.State
	if len(state.Nodes) == 0 {
		return nil, "", fmt.Errorf("bank %q is empty; `muninn index %s`", b.Name, b.Name)
	}
	muted := make(map[string]bool, len(b.Search.Mute))
	for _, id := range b.Search.Mute {
		muted[id] = true
	}
	keep := func(id string) bool {
		n := state.Nodes[id]
		if n == nil || muted[id] || (o.Kind != "" && n.Kind != o.Kind) || !strings.HasPrefix(n.Connector, o.Owner) || (!o.Since.IsZero() && n.At.Before(o.Since)) {
			return false
		}
		for k, v := range o.Filters {
			if n.Attrs[k] != v {
				return false
			}
		}
		return true
	}

	if o.From != "" {
		from := state.Nodes[o.From]
		if from == nil {
			return nil, "", fmt.Errorf("bank %q has no node %q", b.Name, o.From)
		}
		around := walk(b, state, []Hit{{Node: from, Score: 1}}, keep)
		// No text was matched, so the mass is all there is to go by: what holds
		// less than a share of the most that a text node holds came by way of a
		// hub — the repository every branch is of — and is not about this node.
		var most float64
		for _, h := range around {
			if h.Node.ID != o.From && !Structural(h.Node) {
				most = max(most, h.Mass)
			}
		}
		around = slices.DeleteFunc(around, func(h Hit) bool {
			return h.Node.ID == o.From || (!Structural(h.Node) && h.Mass < most*reach)
		})
		return classes(b, state, around, o.K), "", nil
	}

	var lexical []index.Scored
	if c.Lexical != nil {
		for _, s := range c.Lexical.Search(query) {
			if keep(s.Ref.Node) && s.Ref.Idx < len(state.Nodes[s.Ref.Node].Chunks) {
				lexical = append(lexical, s)
			}
		}
	}

	dense, err := c.denseScores(ctx, b, query, keep)
	if err != nil {
		warning = fmt.Sprintf("the embedder could not be reached, so this answer is by the query's words alone and misses what is phrased otherwise or written in another language: %v", err)
	}

	fused := map[index.Ref]float64{}
	for _, channel := range [][]index.Scored{dense, lexical} {
		for rank, s := range channel[:min(len(channel), channelDepth)] {
			fused[s.Ref] += 1 / float64(rrfK+rank+1)
		}
	}
	ranked := make([]index.Scored, 0, len(fused))
	for ref, sc := range fused {
		ranked = append(ranked, index.Scored{Ref: ref, Score: sc})
	}
	index.SortScored(ranked)

	byNode := map[string]*Hit{}
	for _, s := range ranked {
		n := state.Nodes[s.Ref.Node]
		c := n.Chunks[s.Ref.Idx]
		if h, ok := byNode[n.ID]; ok {
			if len(h.Also) < maxAlso {
				h.Also = append(h.Also, c)
			}
			continue
		}
		byNode[n.ID] = &Hit{Node: n, Chunk: c, Score: s.Score}
	}
	for _, h := range byNode {
		hits = append(hits, *h)
	}
	sortHits(hits)
	if !o.NoGraph && len(hits) > 0 {
		hits = walk(b, state, hits, keep)
	}
	if o.HalfLife > 0 {
		now := time.Now()
		for i := range hits {
			if at := hits[i].Node.At; !at.IsZero() && at.Before(now) {
				decay := math.Pow(0.5, float64(now.Sub(at))/float64(o.HalfLife))
				hits[i].Score *= decay
				hits[i].Mass *= decay
			}
		}
		sortHits(hits)
	}
	return classes(b, state, hits, o.K), warning, nil
}

func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Mass != b.Mass {
			return a.Mass > b.Mass
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Node.ID < b.Node.ID
	})
}

// walk seeds the PPR with the top fused hits, weighted by their fused scores,
// and returns the text hits together with the kept nodes the walk reached,
// ranked by mass. Text hits the walk left without mass follow in fused order.
// The walk itself crosses the whole graph; keep decides only what is returned.
func walk(b *bank.Bank, state *index.State, hits []Hit, keep func(string) bool) []Hit {
	seeds := hits[:min(len(hits), b.Search.Seeds)]
	var total float64
	for _, h := range seeds {
		total += h.Score
	}
	personalization := make(map[string]float64, len(seeds))
	order := make([]string, len(seeds))
	for i := range seeds {
		seeds[i].Seed = true
		personalization[seeds[i].Node.ID] = seeds[i].Score / total
		order[i] = seeds[i].Node.ID
	}
	mass := newWalkGraph(state, b.Search).ppr(personalization, order)

	for i := range hits {
		hits[i].Mass = mass[hits[i].Node.ID]
		delete(mass, hits[i].Node.ID)
	}
	for id, m := range mass {
		if !keep(id) {
			continue
		}
		h := Hit{Node: state.Nodes[id], Mass: m}
		if len(h.Node.Chunks) > 0 {
			h.Chunk = h.Node.Chunks[0]
		}
		hits = append(hits, h)
	}
	sortHits(hits)
	return hits
}

// Structural reports a node that is an answer by position in the graph and not
// by its text: one with no text, or one of the semantic layer, whose text is
// only its name. Such hits are listed after the text hits, as one line.
func Structural(n *index.Node) bool {
	return len(n.Chunks) == 0 || n.Connector == index.SemanticConnector
}

// classes cuts the ranking into the bank's classes, each ranked on its own and
// taking at most k places: a bank holds many more turns of talk than guides,
// and one ranking would answer with talk alone. A class is answered when one
// of its hits matched the text nearly as well as the best hit did, and within
// it a hit stays that did, or that the walk left with a fair share of the
// class's mass. The bank's classes come first, in its order, then the others
// by their best hit.
//
// Structural nodes, those with no text, come last and take at most k/4 of the
// places and at least one: a hub collects mass from every seed under it, so it
// can be an answer, and does not get to be the first one.
func classes(b *bank.Bank, state *index.State, hits []Hit, k int) []Hit {
	classOf := b.Classes.Of()
	var best float64
	for _, h := range hits {
		best = max(best, h.Score)
	}
	var order []string
	byClass := map[string][]Hit{}
	matched := map[string]bool{}
	var structural []Hit
	for _, h := range hits {
		if Structural(h.Node) {
			structural = append(structural, h)
			continue
		}
		h.Class = classOf(h.Node.Kind, h.Node.ID)
		if _, ok := byClass[h.Class]; !ok {
			order = append(order, h.Class)
		}
		byClass[h.Class] = append(byClass[h.Class], h)
		matched[h.Class] = matched[h.Class] || h.Score >= best*weak
	}
	rank := map[string]int{}
	for i, name := range b.Classes.Names() {
		rank[name] = i - len(b.Classes)
	}
	for i, name := range order {
		if _, ok := rank[name]; !ok {
			rank[name] = i
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return rank[order[i]] < rank[order[j]] })

	var out []Hit
	for _, name := range order {
		if !matched[name] {
			continue
		}
		in := byClass[name]
		n := 0
		for _, h := range in {
			if (k > 0 && n == k) || (h.Score < best*weak && h.Mass <= in[0].Mass*weak) {
				continue
			}
			out = append(out, h)
			n++
		}
	}
	if k > 0 {
		structural = structural[:min(len(structural), max(1, k/4))]
	}
	out = append(out, structural...)
	sources(state, out)
	return out
}

// sources counts, for the hits that are items of an enrichment, the nodes that
// state them.
func sources(state *index.State, hits []Hit) {
	at := map[string]int{}
	for i, h := range hits {
		if index.Enrichment(h.Node) != "" {
			at[h.Node.ID] = i
		}
	}
	if len(at) == 0 {
		return
	}
	for _, e := range state.Edges {
		if i, ok := at[e.To]; ok && e.Connector == hits[i].Node.Connector {
			hits[i].Sources++
		}
	}
}

func (c *Corpus) denseScores(ctx context.Context, b *bank.Bank, query string, keep func(string) bool) ([]index.Scored, error) {
	if c.VectorsErr != nil {
		return nil, c.VectorsErr
	}
	state, vectors := c.State, c.Vectors
	emb, err := embed.New(b.Embedder)
	if err != nil {
		return nil, err
	}
	if vectors.Len() == 0 {
		return nil, errors.New("no vectors under the bank's embedder yet")
	}
	q, tokens, err := emb.EmbedQuery(ctx, query)
	b.RecordUsage(usage.EmbedQuery, b.Embedder.String(), tokens)
	if err != nil {
		return nil, err
	}
	var out []index.Scored
	for id, n := range state.Nodes {
		if !keep(id) {
			continue
		}
		for i, c := range n.Chunks {
			row := vectors.Row(c.Hash)
			if len(row) != len(q) {
				continue
			}
			var dot float32
			for j, x := range row {
				dot += x * q[j]
			}
			out = append(out, index.Scored{Ref: index.Ref{Node: id, Idx: i}, Score: float64(dot)})
		}
	}
	index.SortScored(out)
	return out, nil
}
