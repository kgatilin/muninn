package semantic

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
)

func textID(i int) string { return fmt.Sprintf("/t/%04d.md", i) }

// newTexts is a state of n text nodes and one structural hub they all hang
// off, which the layer must neither see nor touch.
func newTexts(n int) *index.State {
	s := &index.State{Nodes: map[string]*index.Node{}, Edges: map[string]*index.Edge{}, Semantic: map[string]string{}}
	s.Nodes["/t"] = &index.Node{ID: "/t", Kind: "directory", Connector: "src"}
	for i := 0; i < n; i++ {
		s.Nodes[textID(i)] = index.NewTextNode(textID(i), "document", "src", fmt.Sprintf("text %d", i), nil)
		e := &index.Edge{From: "/t", To: textID(i), Kind: "contains", Connector: "src"}
		s.Edges[e.Key()] = e
	}
	return s
}

// grow attaches every text node to `per` entities, each an old one picked in
// proportion to its degree or, one time in four, a new one: a bipartite graph
// with a heavy-tailed entity side, which is what a healthy layer looks like.
func grow(t *testing.T, g *Gate, texts, per int, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	var ends []string
	entities := 0
	for i := 0; i < texts; i++ {
		var p Patch
		picked := map[string]bool{}
		for len(picked) < per {
			var name string
			if len(ends) == 0 || r.Intn(4) == 0 {
				name = fmt.Sprintf("e%04d", entities)
				entities++
				p.Ops = append(p.Ops, AddEntity(name))
			} else {
				name = ends[r.Intn(len(ends))]
			}
			if !picked[name] {
				picked[name] = true
				p.Ops = append(p.Ops, AddMention(textID(i), EntityID(name), 0))
			}
		}
		if ev, err := g.Propose(p); err != nil || ev.Decision != Accept {
			t.Fatalf("growing the base: %v %+v", err, ev)
		}
		for name := range picked {
			ends = append(ends, name)
		}
		sort.Strings(ends[len(ends)-per:])
	}
}

func cloneState(s *index.State) *index.State {
	out := &index.State{Nodes: map[string]*index.Node{}, Edges: map[string]*index.Edge{}, Semantic: map[string]string{}}
	for id, n := range s.Nodes {
		c := *n
		c.Chunks = append([]index.Chunk(nil), n.Chunks...)
		if n.Attrs != nil {
			c.Attrs = map[string]string{}
			for k, v := range n.Attrs {
				c.Attrs[k] = v
			}
		}
		out.Nodes[id] = &c
	}
	for k, e := range s.Edges {
		c := *e
		out.Edges[k] = &c
	}
	return out
}

// rejectAll measures like the real checks and then says no, so every patch is
// applied, measured and undone.
type rejectAll struct{}

func (rejectAll) Kind() string    { return "reject_all" }
func (rejectAll) Version() string { return "test" }
func (c rejectAll) Evaluate(before, after Snapshot) Result {
	return Result{Check: "reject_all", Decision: Reject}
}

// randomPatch is a valid patch over the layer as it stands, using all five
// operations. Validity is judged against a scratch copy the operations are
// tried on, since a later operation depends on the ones before it.
func randomPatch(r *rand.Rand, l *Layer, serial int) Patch {
	scratch := NewLayer(cloneState(l.state))
	var p Patch
	try := func(op Op) {
		scratch.begin()
		if err := scratch.apply(op); err != nil {
			scratch.rollback()
		} else {
			p.Ops = append(p.Ops, op)
		}
		scratch.end()
	}
	entities := func() []string {
		var ids []string
		for id, n := range scratch.state.Nodes {
			if IsSemantic(n) && n.Kind == KindEntity {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids
	}
	mentioning := func(entity string) []string {
		var ids []string
		for _, v := range scratch.Neighbors(entity) {
			if IsText(scratch.state.Nodes[v]) {
				ids = append(ids, v)
			}
		}
		return ids
	}
	texts := scratch.counts.Texts
	for i := 0; i < 2+r.Intn(6); i++ {
		es := entities()
		switch k := r.Intn(6); {
		case k == 0 || len(es) < 2:
			try(AddEntity(fmt.Sprintf("New Entity %d-%d", serial, i), "alias"))
		case k <= 2:
			try(AddMention(textID(r.Intn(texts)), es[r.Intn(len(es))], float64(r.Intn(3))))
		case k == 3:
			a, b := es[r.Intn(len(es))], es[r.Intn(len(es))]
			try(MergeEntities(a, b))
		default:
			e := es[r.Intn(len(es))]
			ms := mentioning(e)
			if len(ms) == 0 {
				continue
			}
			r.Shuffle(len(ms), func(i, j int) { ms[i], ms[j] = ms[j], ms[i] })
			subset := ms[:1+r.Intn(len(ms))]
			if k == 4 {
				try(SplitEntity(e, fmt.Sprintf("split %d-%d", serial, i), subset...))
			} else {
				try(InsertIntermediate(e, fmt.Sprintf("topic %d-%d", serial, i), subset...))
			}
		}
	}
	return p
}

func measureAll(state *index.State) string {
	v := WholeView(state, true)
	return fmt.Sprint(ScaleFree(v, 20, 5, 0), Fractal(v, 20, 32, 6))
}

// The hard property: a patch applied and undone leaves the graph, the
// adjacency, the counters and every metric exactly as they were — for all five
// operations, and also when the patch fails half way through.
func TestUndoRestoresTheGraphExactly(t *testing.T) {
	kinds := map[OpKind]int{}
	for seed := int64(1); seed <= 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		state := newTexts(60)
		open := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 2000}
		grow(t, open, 60, 3, seed)
		// Some accepted restructuring first, so later patches meet topics,
		// aliases and merged entities.
		for i := 0; i < 5; i++ {
			if p := randomPatch(r, open.Layer, i); len(p.Ops) > 0 {
				if _, err := open.Propose(p); err != nil {
					t.Fatalf("seed %d: a valid patch failed: %v", seed, err)
				}
			}
		}
		if got, want := open.Layer.Counts(), NewLayer(state).Counts(); got != want {
			t.Fatalf("seed %d: live counters %+v, recomputed %+v", seed, got, want)
		}

		want, wantMetrics := cloneState(state), measureAll(state)
		wantCounts, wantAdj := open.Layer.Counts(), fmt.Sprint(open.Layer.adj)
		closed := &Gate{Layer: open.Layer, Checks: []Check{rejectAll{}}, Hops: 2, RegionCap: 2000}
		for i := 0; i < 10; i++ {
			p := randomPatch(r, closed.Layer, 100+i)
			if len(p.Ops) == 0 {
				continue
			}
			for _, op := range p.Ops {
				kinds[op.Kind]++
			}
			broken := i%3 == 2
			if broken {
				p.Ops = append(p.Ops, AddMention("/no/such/node", p.Ops[0].Entity, 0))
			}
			ev, err := closed.Propose(p)
			if broken != (err != nil) || ev.Decision != Reject {
				t.Fatalf("seed %d patch %d: decision %s, err %v", seed, i, ev.Decision, err)
			}
			if !reflect.DeepEqual(state.Nodes, want.Nodes) || !reflect.DeepEqual(state.Edges, want.Edges) {
				t.Fatalf("seed %d patch %d (%s): the graph differs after undo", seed, i, p.Summary())
			}
			if got := closed.Layer.Counts(); got != wantCounts {
				t.Fatalf("seed %d patch %d: counters %+v, want %+v", seed, i, got, wantCounts)
			}
			if got := fmt.Sprint(closed.Layer.adj); got != wantAdj {
				t.Fatalf("seed %d patch %d: the adjacency differs after undo", seed, i)
			}
			if got := measureAll(state); got != wantMetrics {
				t.Fatalf("seed %d patch %d: metrics %s, want %s", seed, i, got, wantMetrics)
			}
		}
	}
	for _, k := range []OpKind{OpAddEntity, OpAddMention, OpMergeEntities, OpSplitEntity, OpInsertIntermediate} {
		if kinds[k] == 0 {
			t.Errorf("no undone patch held a %s", k)
		}
	}
	t.Logf("operations undone: %v", kinds)
}

func TestOperations(t *testing.T) {
	state := newTexts(6)
	g := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 100}
	must := func(p Patch) {
		t.Helper()
		if ev, err := g.Propose(p); err != nil || ev.Decision != Accept {
			t.Fatalf("%s: %v %+v", p.Summary(), err, ev)
		}
	}
	must(Patch{Ops: []Op{AddEntity("Event  Log"), AddEntity("journal"),
		AddMention(textID(0), "entity:event log", 0), AddMention(textID(1), "entity:event log", 0),
		AddMention(textID(1), "entity:journal", 0), AddMention(textID(2), "entity:journal", 0)}})
	if n := state.Nodes["entity:event log"]; n == nil || n.Kind != KindEntity || n.Connector != index.SemanticConnector || n.Chunks[0].Text != "Event  Log" {
		t.Fatalf("entity = %+v", n)
	}

	must(Patch{Ops: []Op{MergeEntities("entity:journal", "entity:event log")}})
	kept := state.Nodes["entity:event log"]
	if state.Nodes["entity:journal"] != nil || kept.Attrs["aliases"] != "journal" || !strings.Contains(kept.Chunks[0].Text, "journal") {
		t.Errorf("after the merge: %+v", kept)
	}
	if got := g.Layer.Neighbors("entity:event log"); len(got) != 3 {
		t.Errorf("the kept entity is mentioned by %v", got)
	}

	must(Patch{Ops: []Op{InsertIntermediate("entity:event log", "append only", textID(0), textID(1))}})
	if got := g.Layer.Neighbors("topic:append only"); !reflect.DeepEqual(got, []string{textID(0), textID(1), "entity:event log"}) {
		t.Errorf("the topic's neighbours: %v", got)
	}
	if got := g.Layer.Neighbors("entity:event log"); !reflect.DeepEqual(got, []string{textID(2), "topic:append only"}) {
		t.Errorf("the entity's neighbours: %v", got)
	}

	must(Patch{Ops: []Op{SplitEntity("entity:event log", "audit log", textID(2))}})
	if got := g.Layer.Neighbors("entity:audit log"); !reflect.DeepEqual(got, []string{textID(2)}) {
		t.Errorf("the split entity's neighbours: %v", got)
	}
	if c := g.Layer.Counts(); c != (Counts{Texts: 6, Entities: 2, Topics: 1, Edges: 4, Singletons: 2}) {
		t.Errorf("counts = %+v", c)
	}

	// The structural edges are as they were, and a connector's node is not a
	// thing a patch may touch.
	if _, err := g.Propose(Patch{Ops: []Op{AddMention(textID(0), "/t", 0)}}); err == nil {
		t.Error("a mention of a connector's node was applied")
	}
	structural := 0
	for _, e := range state.Edges {
		if e.Connector == "src" {
			structural++
		}
	}
	if structural != 6 {
		t.Errorf("%d structural edges left of 6", structural)
	}

	// Text nodes that lose their last mention leave nothing behind.
	g.Layer.DropEdgesOf(textID(2))
	if swept := g.Layer.Sweep(1); swept != 1 || state.Nodes["entity:audit log"] != nil {
		t.Errorf("swept %d", swept)
	}
	if nodes, edges := Reset(state); nodes != 2 || edges != 3 || len(state.Nodes) != 7 || len(state.Edges) != 6 {
		t.Errorf("reset took %d nodes and %d edges; %d nodes and %d edges left", nodes, edges, len(state.Nodes), len(state.Edges))
	}
}

// One entity attached to everything is the hairball the gate is for: the
// region had a dimension, and after the patch it is within two hops of itself.
func TestGateRejectsAHub(t *testing.T) {
	const texts = 400
	state := newTexts(texts)
	open := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 5000}
	grow(t, open, texts, 3, 11)

	// The grown layer holds more entities than the default budget allows a
	// bank of this size; this test is about the hub.
	checks, err := BuildChecks(bank.Semantic{Checks: map[string]bank.Check{"entity_budget": {Params: map[string]float64{"ratio": 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gate{Layer: open.Layer, Checks: checks, Hops: 2, RegionCap: 5000}
	before := cloneState(state)

	hub := Patch{Ops: []Op{AddEntity("agent")}}
	for i := 0; i < texts; i++ {
		hub.Ops = append(hub.Ops, AddMention(textID(i), "entity:agent", 0))
	}
	ev, err := g.Propose(hub)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ev.Checks {
		t.Logf("%-16s %-14s before %s %.3f, after %s %.3f, loss %.3f → %.3f  %s", r.Check, r.Decision,
			r.Before.Status, r.Before.Value, r.After.Status, r.After.Value, r.LossBefore, r.LossAfter, r.Reason)
	}
	if ev.Decision != Reject || len(ev.Violations()) == 0 {
		t.Fatalf("the hub was accepted: %+v", ev)
	}
	if !reflect.DeepEqual(state.Nodes, before.Nodes) || !reflect.DeepEqual(state.Edges, before.Edges) {
		t.Fatal("a rejected patch left something behind")
	}

	// The same gate lets an ordinary patch through.
	small := Patch{Ops: []Op{AddEntity("ravens"), AddMention(textID(1), "entity:ravens", 0), AddMention(textID(2), "entity:ravens", 0)}}
	if ev, err := g.Propose(small); err != nil || ev.Decision != Accept {
		for _, r := range ev.Checks {
			t.Logf("%-16s %-14s %.3f → %.3f  %s", r.Check, r.Decision, r.Before.Value, r.After.Value, r.Reason)
		}
		t.Fatalf("an ordinary patch was rejected: %v", err)
	}

	// A region too small to measure blocks nothing, the hub included.
	tiny := &Gate{Layer: NewLayer(newTexts(8)), Checks: checks, Hops: 2, RegionCap: 5000}
	p := Patch{Ops: []Op{AddEntity("agent")}}
	for i := 0; i < 8; i++ {
		p.Ops = append(p.Ops, AddMention(textID(i), "entity:agent", 0))
	}
	ev, err = tiny.Propose(p)
	if err != nil || ev.Decision != Accept {
		t.Fatalf("a patch over an unmeasurable region: %v %+v", err, ev)
	}
	for _, r := range ev.Checks {
		if (r.Check == "scale_free" || r.Check == "fractal") && r.Decision != NotApplicable {
			t.Errorf("%s on 9 nodes: %s", r.Check, r.Decision)
		}
	}
}

func TestBudgetAndSingletonChecks(t *testing.T) {
	on := true
	checks, err := BuildChecks(bank.Semantic{Checks: map[string]bank.Check{
		"entity_budget":   {Params: map[string]float64{"allowance": 2, "ratio": 0.5}},
		"singleton_share": {Enabled: &on, Params: map[string]float64{"ceiling": 0.5, "min_entities": 2}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gate{Layer: NewLayer(newTexts(4)), Checks: checks, Hops: 2, RegionCap: 100}
	// Four text nodes allow 2 + 0.5·4 = 4 entities.
	var p Patch
	for i := 0; i < 4; i++ {
		p.Ops = append(p.Ops, AddEntity(fmt.Sprintf("e%d", i)), AddMention(textID(i), EntityID(fmt.Sprintf("e%d", i)), 0), AddMention(textID((i+1)%4), EntityID(fmt.Sprintf("e%d", i)), 0))
	}
	if ev, err := g.Propose(p); err != nil || ev.Decision != Accept {
		t.Fatalf("within budget: %v %+v", err, ev)
	}
	ev, _ := g.Propose(Patch{Ops: []Op{AddEntity("fifth"), AddMention(textID(0), "entity:fifth", 0), AddMention(textID(1), "entity:fifth", 0)}})
	if v := ev.Violations(); len(v) != 1 || v[0].Check != "entity_budget" || v[0].Delta != 1 {
		t.Errorf("over budget: %+v", ev.Checks)
	}
	// Four singletons among eight entities is the ceiling; the fifth is over it.
	g.Checks = g.Checks[1:]
	for i := 0; i < 5; i++ {
		ev, _ := g.Propose(Patch{Ops: []Op{AddEntity(fmt.Sprintf("s%d", i)), AddMention(textID(0), EntityID(fmt.Sprintf("s%d", i)), 0)}})
		if want := map[bool]string{true: Accept, false: Reject}[i < 4]; ev.Decision != want {
			t.Errorf("singleton %d: %s, want %s (%+v)", i, ev.Decision, want, ev.Checks[0])
		}
	}
}

// The baseline is read through the open transaction, from the graph as the
// patch left it and the difference the patch made. It has to be the graph
// that was there.
func TestTheBaselineViewIsTheGraphBeforeThePatch(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		state := newTexts(50)
		g := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 2000}
		grow(t, g, 50, 3, seed)
		l := g.Layer
		for i := 0; i < 5; i++ {
			p := randomPatch(r, l, i)
			if len(p.Ops) == 0 {
				continue
			}
			l.begin()
			for _, op := range p.Ops {
				if err := l.apply(op); err != nil {
					t.Fatal(err)
				}
			}
			region := l.region(p.footprint(), 2, 2000)
			virtual, counts := l.view(region, true), l.tx.before
			l.rollback()
			l.end()
			actual := l.view(region, false)
			if !reflect.DeepEqual(virtual.Nodes, actual.Nodes) || !reflect.DeepEqual(virtual.Degree, actual.Degree) || !reflect.DeepEqual(virtual.Adj, actual.Adj) {
				t.Fatalf("seed %d patch %d (%s): the baseline read through the patch is not the graph before it", seed, i, p.Summary())
			}
			if counts != l.Counts() {
				t.Fatalf("seed %d patch %d: baseline counters %+v, the graph's %+v", seed, i, counts, l.Counts())
			}
		}
	}
}
