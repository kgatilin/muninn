// Package enrich is the enrichment stage: a bank names enrichments, each a
// selection of the graph's nodes, a prompt and the kind of node an answer
// becomes, and the stage has a chat model draw items from the selected nodes —
// the rules a user gave the agents out of conversation turns, the rules of the
// architecture out of design documents.
//
// Nodes are read in neighbourhoods: a node with the selected nodes nearest to
// it by their vectors. What the model draws from a neighbourhood names the
// nodes of it that support each item, and every one of them gets an edge to
// the item, so an item ties passages together as an entity does. The items of
// an enrichment are then settled one neighbourhood after another in the order
// of time: an item that says what a kept one says adds its edges to it, one
// that changes a kept item rewrites it under the same id, one that cancels it
// removes it.
//
// The stage is not part of the semantic layer and does not pass its gate:
// items are few by the enrichment's ratio, not by a measured shape. They
// belong to index.EnrichConnector(name) and are text like any connector's.
package enrich

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/llm"
	"github.com/kgatilin/muninn/internal/usage"
)

const (
	// EdgeStates is node → item, stated by the stage: the node supports it.
	EdgeStates = "states"
	// EdgeInjected is turn → item, stated by a session connector: the turn's
	// agent was shown the item. A correction in that session is first taken
	// as being about one of those.
	EdgeInjected = "injected"

	// version is part of what State.Enriched records for a node read; a change
	// to the frame of a prompt changes it, and the next run reads again.
	version = "v7"

	concurrency = 24
	// ahead is how many neighbourhoods may be read and not yet settled: what
	// a stopped run reads again.
	ahead = 96
	// batch is how many neighbourhoods are read between two saves.
	batch = 16
	// memberBudget is the runes of a node the model reads, contextBudget those
	// of the node a `next` edge puts before it: a message often answers the end
	// of the reply before it.
	memberBudget  = 12000
	contextBudget = 2000
	// candidates is how many kept items, the nearest by vector, an item is
	// settled against.
	candidates = 12
	// shown is the most kept items one settling call is given.
	shown = 60
	// floor is the fewest items an enrichment may keep whatever its ratio.
	floor = 10
	// slack is how far over its cap an enrichment gets before a run stops to
	// press it: a pass is a call for every few items, too dear for every batch.
	slack = 1.25
	// itemLimit is the runes over which a pressed pass leaves an item alone.
	itemLimit = 900
)

// Stats is what one run did for one enrichment.
type Stats struct {
	Selected, Pending, Groups int
	Added, Updated, Same      int
	Retired                   int
	// Merged are the kept items that went into another.
	Merged int
}

// Stage is the stage as index.Options.After takes it, or nil for a bank with
// no model or no enrichment.
func Stage(b *bank.Bank) func(context.Context, *index.Session) error {
	ready := false
	for _, e := range b.Enrich.Sets {
		ready = ready || e.Missing() == ""
	}
	if b.Enrich.Model == "" || !ready {
		return nil
	}
	return func(ctx context.Context, s *index.Session) error {
		spec, err := llm.ParseSpec(b.Enrich.Model)
		if err != nil {
			return fmt.Errorf("enrich.model: %w", err)
		}
		spec.Endpoint = b.Enrich.Endpoint
		client, err := llm.New(spec)
		if err != nil {
			return fmt.Errorf("enrichment stage: %w", err)
		}
		_, err = Run(ctx, s, client)
		return err
	}
}

// Reset drops the items of the named enrichment, or of all with "", and the
// record of the nodes read for them.
func Reset(state *index.State, name string) (nodes int) {
	mine := func(connector string) bool {
		n := index.Enrichment(&index.Node{Connector: connector})
		return n != "" && (name == "" || n == name)
	}
	for id, n := range state.Nodes {
		if mine(n.Connector) {
			delete(state.Nodes, id)
			nodes++
		}
	}
	for k, e := range state.Edges {
		if mine(e.Connector) {
			delete(state.Edges, k)
		}
	}
	for k := range state.Enriched {
		if set, _, _ := strings.Cut(k, "\x00"); name == "" || set == name {
			delete(state.Enriched, k)
		}
	}
	return nodes
}

// Run runs every enrichment of the bank, by name.
func Run(ctx context.Context, s *index.Session, client llm.Client) (map[string]Stats, error) {
	state := s.State()
	if state.Enriched == nil {
		state.Enriched = map[string]string{}
	}
	for k := range state.Enriched {
		if _, id, _ := strings.Cut(k, "\x00"); state.Nodes[id] == nil {
			delete(state.Enriched, k)
		}
	}
	names := make([]string, 0, len(s.Bank().Enrich.Sets))
	for name, e := range s.Bank().Enrich.Sets {
		if missing := e.Missing(); missing != "" {
			fmt.Fprintf(s.Log(), "enrich %s: left out, it needs %s\n", name, missing)
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := map[string]Stats{}
	var failed error
	for _, name := range names {
		x := &run{s: s, client: client, state: state, name: name, cfg: s.Bank().Enrich.Sets[name].Resolved(), owner: index.EnrichConnector(name)}
		st, err := x.run(ctx)
		out[name] = st
		if err != nil {
			failed = fmt.Errorf("enrich.%s: %w", name, err)
			break
		}
	}
	_, err := s.Commit(ctx)
	return out, errors.Join(failed, err)
}

type run struct {
	s      *index.Session
	client llm.Client
	state  *index.State
	name   string
	cfg    bank.Enrichment
	owner  string

	selected []*index.Node
	vector   map[string][]float32 // a selected node's, and a kept item's
	prev     map[string]string    // the node a `next` edge puts before a node
	injected map[string]map[string]bool
	signed   map[string]string
	// pressedTo is how many items the last pressed pass of the run left.
	pressedTo int
	// before is the node a `next` edge puts before a node.
	before map[string]*index.Node
}

// group is one neighbourhood: the node that called it, and the nodes read
// with it, all in the order of their time.
type group struct {
	seed    *index.Node
	members []*index.Node
	items   []item
	err     error
	// done is closed when the neighbourhood has been read.
	done chan struct{}
}

type item struct {
	text   string
	from   []*index.Node
	fields map[string]string
}

func (x *run) key(id string) string { return x.name + "\x00" + id }

// signature is what the record holds for a node read: what was shown of it,
// and everything that decides what is drawn from it.
func (x *run) signature(n *index.Node) string {
	if x.signed == nil {
		x.signed = map[string]string{}
	}
	if x.signed[n.ID] == "" {
		x.signed[n.ID] = sum(x.show(n)) + " " + version + " " + x.client.ID() + " " + sum(x.cfg.Prompt + "\x00" + strings.Join(x.cfg.Fields, ","))[:12]
	}
	return x.signed[n.ID]
}

func (x *run) run(ctx context.Context) (Stats, error) {
	var st Stats
	log := x.s.Log()
	x.vector, x.prev, x.injected = map[string][]float32{}, map[string]string{}, map[string]map[string]bool{}
	for _, e := range x.state.Edges {
		switch e.Kind {
		case "next":
			x.prev[e.To] = e.From
		case EdgeInjected:
			if turn := x.state.Nodes[e.From]; turn != nil {
				session := turn.Attrs["session"]
				if x.injected[session] == nil {
					x.injected[session] = map[string]bool{}
				}
				x.injected[session][e.To] = true
			}
		}
	}
	// An item is as late as the latest node behind it, and a node's time may
	// have been corrected since the item was written.
	latest := map[string]time.Time{}
	for _, e := range x.state.Edges {
		if from := x.state.Nodes[e.From]; e.Kind == EdgeStates && e.Connector == x.owner && from != nil && from.At.After(latest[e.To]) {
			latest[e.To] = from.At
		}
	}
	for id, at := range latest {
		if n := x.state.Nodes[id]; n != nil {
			n.At = at
		}
	}
	// A node's signature holds what is shown of it, the node before it too, so
	// this comes before anything is signed.
	x.before = map[string]*index.Node{}
	for id, prev := range x.prev {
		x.before[id] = x.state.Nodes[prev]
	}
	selects := x.cfg.Selector()
	for _, n := range x.state.Nodes {
		switch {
		case index.Enrichment(n) != "":
			// Its own items, and those of the bank's other enrichments: an item may
			// speak against one of theirs.
			x.vector[n.ID] = x.mean(n)
		case n.Connector == index.SemanticConnector || len(n.Chunks) == 0:
		case selects(n.Kind, n.ID):
			x.selected = append(x.selected, n)
			x.vector[n.ID] = x.mean(n)
		}
	}
	byTime(x.selected)
	st.Selected = len(x.selected)

	// A neighbourhood is called by the oldest node not read yet, and the nodes
	// read with it count as read: a node is the subject of one call.
	taken := map[string]bool{}
	var groups []*group
	for _, n := range x.selected {
		if taken[n.ID] || x.state.Enriched[x.key(n.ID)] == x.signature(n) {
			continue
		}
		st.Pending++
		g := &group{seed: n, members: append([]*index.Node{n}, x.nearest(n)...)}
		byTime(g.members)
		for _, m := range g.members {
			if !taken[m.ID] && m != n && x.state.Enriched[x.key(m.ID)] != x.signature(m) {
				st.Pending++
			}
			taken[m.ID] = true
		}
		groups = append(groups, g)
	}
	st.Groups = len(groups)
	if len(groups) == 0 {
		return st, nil
	}
	fmt.Fprintf(log, "enrich %s: %d of %d selected nodes to read, in %d neighbourhoods\n", x.name, st.Pending, st.Selected, st.Groups)

	// Reading does not wait for settling: the neighbourhoods are read ahead,
	// `concurrency` at once and no further than `ahead` past the batch being
	// settled, while the batches are settled one after another. What a reader
	// needs of the graph — x.before — was taken at the start, the graph being
	// written under it.
	system, schema := x.extractPrompt()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	slots := make(chan struct{}, concurrency)
	window := make(chan struct{}, ahead)
	for _, g := range groups {
		g.done = make(chan struct{})
	}
	go func() {
		for _, g := range groups {
			select {
			case window <- struct{}{}:
			case <-ctx.Done():
				g.err = ctx.Err()
				close(g.done)
				continue
			}
			slots <- struct{}{}
			go func() {
				defer close(g.done)
				defer func() { <-slots }()
				if g.err = ctx.Err(); g.err == nil {
					g.items, g.err = x.extract(ctx, system, schema, g)
				}
			}()
		}
	}()
	for start := 0; start < len(groups); start += batch {
		part := groups[start:min(start+batch, len(groups))]
		for _, g := range part {
			<-g.done
			select {
			case <-window:
			default: // a neighbourhood the run was stopped before reading
			}
		}
		// Well over the ratio, what is kept is pressed together before more is
		// settled against it. Nothing is ever refused for want of room.
		// A pass that could not bring the items down is not tried again until
		// they have grown by as much once more.
		if n := len(x.kept()); float64(n) > slack*float64(max(x.cap(), x.pressedTo)) {
			if err := x.consolidate(ctx, &st, true); err != nil {
				return st, err
			}
			x.pressedTo = len(x.kept())
		}
		// A failed request is the run's: what was settled before it is kept,
		// and the neighbourhoods from it on stay pending.
		var failed error
		var drawn []*group
		for _, g := range part {
			if g.err != nil && !soft(g.err) {
				failed = g.err
				break
			}
			if g.err == nil && len(g.items) > 0 {
				drawn = append(drawn, g)
			}
		}
		// The batch is settled in one call. An answer that cannot be used is
		// asked for again a neighbourhood at a time, where less can go wrong.
		if failed == nil && len(drawn) > 0 {
			if failed = x.settle(ctx, drawn, &st); failed != nil && soft(failed) {
				failed = nil
				for _, g := range drawn {
					if err := x.settle(ctx, []*group{g}, &st); err != nil && !soft(err) {
						failed = err
						break
					}
				}
			}
		}
		if failed == nil {
			for _, g := range part {
				for _, m := range g.members {
					x.state.Enriched[x.key(m.ID)] = x.signature(m)
				}
			}
		}
		if err := x.s.SaveState(); err != nil {
			return st, err
		}
		if failed != nil {
			return st, failed
		}
		fmt.Fprintf(log, "enrich %s: %d/%d neighbourhoods read, %d items kept\n", x.name, min(start+batch, len(groups)), len(groups), len(x.kept()))
	}
	// What a later item overturned goes before the items are pressed together:
	// pressed, the overturned part would be written into a broader item.
	if err := x.consolidate(ctx, &st, false); err != nil {
		return st, err
	}
	if x.over() > 1 {
		if err := x.consolidate(ctx, &st, true); err != nil {
			return st, err
		}
	}
	fmt.Fprintf(log, "enrich %s: %d new, %d updated, %d said again, %d merged away, %d retired; %d items kept, the ratio asks for %d\n", x.name, st.Added, st.Updated, st.Same, st.Merged, st.Retired, len(x.kept()), x.cap())
	return st, nil
}

// mean is a node's vector: the mean of its chunks' vectors, nil when the bank
// holds none for it yet.
func (x *run) mean(n *index.Node) []float32 {
	var out []float32
	count := 0
	for _, c := range n.Chunks {
		row := x.s.Vectors().Row(c.Hash)
		if len(row) == 0 {
			continue
		}
		if out == nil {
			out = make([]float32, len(row))
		}
		if len(row) != len(out) {
			continue
		}
		for i, v := range row {
			out[i] += v
		}
		count++
	}
	if count == 0 {
		return nil
	}
	return unit(out)
}

func unit(v []float32) []float32 {
	var norm float64
	for _, c := range v {
		norm += float64(c) * float64(c)
	}
	if norm == 0 {
		return nil
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(norm))
	}
	return v
}

func dot(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

// nearest are the selected nodes closest to the node, read or not: a node
// that arrives later is read with the older ones it resembles, and what it
// repeats lands on the item they made.
func (x *run) nearest(n *index.Node) []*index.Node {
	type scored struct {
		n *index.Node
		d float64
	}
	var all []scored
	for _, m := range x.selected {
		if m != n {
			if d := dot(x.vector[n.ID], x.vector[m.ID]); d > -1 {
				all = append(all, scored{m, d})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].d != all[j].d {
			return all[i].d > all[j].d
		}
		return all[i].n.ID < all[j].n.ID
	})
	out := make([]*index.Node, 0, x.cfg.Neighbours)
	for _, s := range all[:min(len(all), x.cfg.Neighbours)] {
		out = append(out, s.n)
	}
	return out
}

func byTime(nodes []*index.Node) {
	sort.Slice(nodes, func(i, j int) bool {
		if !nodes[i].At.Equal(nodes[j].At) {
			return nodes[i].At.Before(nodes[j].At)
		}
		return nodes[i].ID < nodes[j].ID
	})
}

// show is what the model reads of a node: its text, and for a part of a
// conversation the end of the part before it.
func (x *run) show(n *index.Node) string {
	body := clip(text(n), memberBudget)
	if p := x.before[n.ID]; p != nil {
		return "(said just before)\n" + tail(text(p), contextBudget) + "\n(the passage)\n" + body
	}
	return body
}

const frame = `

You are given passages, numbered, each with its date. Answer with the items you find, as JSON: {"items": [{"text": "…", "from": [1, 3]%s}]}.

"text" is the item in one or two sentences, in the imperative where it is a rule, complete without the passages: name the thing it is about instead of "it" or "that". Write it in the language of the passages it comes from. "from" lists the numbers of the passages that support it; when passages disagree, the later one holds, and the item says what holds now.%s Give an item once, however many passages repeat it. Passages often hold none: answer with an empty list then.`

func (x *run) extractPrompt() (system, schema string) {
	properties := `"text":{"type":"string"},"from":{"type":"array","items":{"type":"integer"}}`
	required := `"text","from"`
	example, note := "", ""
	for _, f := range x.cfg.Fields {
		properties += fmt.Sprintf(`,%q:{"type":"string"}`, f)
		required += fmt.Sprintf(`,%q`, f)
		example += fmt.Sprintf(`, %q: "…"`, f)
	}
	if len(x.cfg.Fields) > 0 {
		note = " The other fields are short strings, named for what they hold."
	}
	schema = `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{` + properties + `},"required":[` + required + `],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`
	return strings.TrimSpace(x.cfg.Prompt) + fmt.Sprintf(frame, example, note), schema
}

func (x *run) passages(g *group) string {
	var b strings.Builder
	for i, m := range g.members {
		fmt.Fprintf(&b, "--- passage %d · %s · %s\n%s\n\n", i+1, m.At.Format("2006-01-02"), m.ID, x.show(m))
	}
	return b.String()
}

func (x *run) extract(ctx context.Context, system, schema string, g *group) ([]item, error) {
	var out struct {
		Items []map[string]any
	}
	if err := x.complete(ctx, system, x.passages(g), schema, &out); err != nil {
		return nil, err
	}
	var items []item
	for _, raw := range out.Items {
		it := item{fields: map[string]string{}}
		it.text, _ = raw["text"].(string)
		if it.text = strings.TrimSpace(it.text); it.text == "" {
			continue
		}
		seen := map[int]bool{}
		from, _ := raw["from"].([]any)
		for _, v := range from {
			if f, ok := v.(float64); ok && int(f) >= 1 && int(f) <= len(g.members) && !seen[int(f)] {
				seen[int(f)] = true
				it.from = append(it.from, g.members[int(f)-1])
			}
		}
		if len(it.from) < x.cfg.Votes {
			continue
		}
		for _, f := range x.cfg.Fields {
			if v, _ := raw[f].(string); strings.TrimSpace(v) != "" {
				it.fields[f] = strings.TrimSpace(v)
			}
		}
		items = append(items, it)
	}
	return items, nil
}

const settleSchema = `{"type":"object","properties":{"decisions":{"type":"array","items":{"type":"object","properties":{"item":{"type":"integer"},"action":{"type":"string","enum":["new","same","update","merge","retire"]},"id":{"type":"string"},"with":{"type":"array","items":{"type":"string"}},"text":{"type":"string"},"against":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"}},"required":["id","text"],"additionalProperties":false}}},"required":["item","action","id","with","text","against"],"additionalProperties":false}}},"required":["decisions"],"additionalProperties":false}`

const settleSystem = `Items were drawn from passages of a project. You are given the items, numbered, and the items already kept that are nearest to them, each with its id. Kept items marked "shown" were in front of the agent in the conversation the passages come from, so a correction is most likely about one of them.

For each item choose one action:
- "same": a kept item says this already, in these or other words, in this or another language. Give its id. Look for it before anything else: an item kept twice is served twice.
- "update": a kept item is about the same matter and the new one changes, narrows or contradicts it. The new one is later and wins. Give the kept item's id and, in "text", the item as it stands now: one or two sentences, complete in themselves, keeping what of the old one still holds.
- "merge": the new item and two or more kept items are about one matter, and one broader item says what all of them say. Give in "id" the kept item that stays, in "with" the ids of the kept items that go into it, and in "text" the one item that stands for them all: nothing any of them constrains may be lost, so merge items about one matter and never items that merely sit near each other.
- "retire": the new item cancels a kept one and puts nothing in its place. Give its id.
- "new": no kept item is about this matter. Give the text in "text"; "id" is empty.

The items come from several groups of passages and may repeat each other. Settle the first of such items as above, and for each later one answer "same" with the id "item <n>", <n> the number of the first.%s

Two items about related matters are two items: update only an item about the same matter.

"with" is empty for every action but "merge".

Whatever the action, the new item may also speak against a part of other items: a kept item that says several things, one of which the new item cancels or replaces, or an item under "Items of other readings", which are kept by another reading of the project and take no action of yours. Give each such item in "against": its id, and in "text" the item as it stands without what the new one overturns, everything else of it kept word for word — or "" when nothing of it is left. The new item is later and wins. Only a direct contradiction counts: an item that is narrower, broader or about something else is not spoken against. "against" is empty for most items.

Answer with JSON: {"decisions": [{"item": 1, "action": "…", "id": "…", "with": [], "text": "…"}]}`

// itemRef is the id of another item of the batch, as the model is told to
// write it.
var itemRef = regexp.MustCompile(`^item\D{0,2}(\d+)$`)

var idHex = regexp.MustCompile(`[0-9a-f]{12}`)

// cap is how many items the ratio asks for. It is a pressure to merge and
// never a reason to drop: an enrichment over it keeps what it has.
func (x *run) cap() int { return max(floor, int(x.cfg.Ratio*float64(len(x.selected)))) }

// over is the items kept as a share of the cap.
func (x *run) over() float64 { return float64(len(x.kept())) / float64(x.cap()) }

// kept are the enrichment's items.
func (x *run) kept() []*index.Node {
	var out []*index.Node
	for _, n := range x.state.Nodes {
		if n.Connector == x.owner {
			out = append(out, n)
		}
	}
	return out
}

// settle asks what the items of a batch of neighbourhoods are to the items
// kept and to each other, and applies the answer. The kept items shown are
// the nearest to each new one, together.
func (x *run) settle(ctx context.Context, groups []*group, st *Stats) error {
	var items []item
	shownHere := map[string]bool{}
	for _, g := range groups {
		items = append(items, g.items...)
		for _, m := range g.members {
			for id := range x.injected[m.Attrs["session"]] {
				shownHere[id] = true
			}
		}
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.text
	}
	vecs, err := x.embed(ctx, texts)
	if err != nil {
		return err
	}
	kept := x.kept()
	near := map[string]float64{}
	for _, n := range kept {
		near[n.ID] = -1
	}
	shortlist := map[string]bool{}
	for _, v := range vecs {
		byItem := slices.Clone(kept)
		sort.Slice(byItem, func(i, j int) bool {
			a, b := dot(v, x.vector[byItem[i].ID]), dot(v, x.vector[byItem[j].ID])
			if a != b {
				return a > b
			}
			return byItem[i].ID < byItem[j].ID
		})
		for _, n := range byItem[:min(len(byItem), candidates)] {
			shortlist[n.ID] = true
			near[n.ID] = max(near[n.ID], dot(v, x.vector[n.ID]))
		}
	}
	kept = slices.DeleteFunc(kept, func(n *index.Node) bool { return !shortlist[n.ID] && !shownHere[n.ID] })
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if shownHere[a.ID] != shownHere[b.ID] {
			return shownHere[a.ID]
		}
		if near[a.ID] != near[b.ID] {
			return near[a.ID] > near[b.ID]
		}
		return a.ID < b.ID
	})
	room := x.over() < 1

	var b strings.Builder
	b.WriteString("Items:\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. %s\n", i+1, it.text)
	}
	b.WriteString("\nKept items:\n")
	known := map[string]bool{}
	for _, n := range kept[:min(len(kept), shown)] {
		known[n.ID] = true
		mark := ""
		if shownHere[n.ID] {
			mark = " (shown)"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", n.ID, mark, text(n))
	}
	if len(known) == 0 {
		b.WriteString("(none)\n")
	}
	if others := x.others(vecs); len(others) > 0 {
		b.WriteString("\nItems of other readings:\n")
		for _, n := range others {
			fmt.Fprintf(&b, "- %s: %s\n", n.ID, text(n))
		}
	}
	full := ""
	if !room {
		full = "\n\nThe project keeps more items than it should. Say \"same\" or \"update\" wherever a kept item can carry the new one, its text growing to hold both. Where kept items come under one principle, \"merge\" them, with the new item or apart from it, into one broader item that loses nothing. \"new\" is for an item no kept one can carry."
	}
	var out struct {
		Decisions []struct {
			Item     int
			Action   string
			ID, Text string
			With     []string
			Against  []revision
		}
	}
	if err := x.complete(ctx, fmt.Sprintf(settleSystem, full), b.String(), settleSchema, &out); err != nil {
		return err
	}
	log := x.s.Log()
	// What makes room comes before what takes it, and an item said to repeat
	// another of the batch comes after that other has its id.
	rank := func(action, id string) int {
		switch {
		case itemRef.MatchString(id):
			return 2
		case action == "new":
			return 1
		}
		return 0
	}
	sort.SliceStable(out.Decisions, func(i, j int) bool {
		return rank(out.Decisions[i].Action, out.Decisions[i].ID) < rank(out.Decisions[j].Action, out.Decisions[j].ID)
	})
	settled := map[int]string{}
	for _, d := range out.Decisions {
		if d.Item < 1 || d.Item > len(items) {
			continue
		}
		it, vec := items[d.Item-1], vecs[d.Item-1]
		body := strings.TrimSpace(d.Text)
		if m := itemRef.FindStringSubmatch(d.ID); m != nil {
			first, _ := strconv.Atoi(m[1])
			d.Action, d.ID = "same", settled[first]
			known[d.ID] = d.ID != "" && x.state.Nodes[d.ID] != nil
		}
		// The id comes back as the model copied it: with the mark beside it,
		// or without its prefix. One that is still not a kept item's was made
		// up, and the item stands as a new one.
		if hex := idHex.FindString(d.ID); hex != "" {
			d.ID = x.cfg.Kind + ":" + hex
		}
		if d.Action != "new" && !known[d.ID] {
			d.Action, body = "new", it.text
		}
		fmt.Fprintf(log, "enrich %s: %s %s: %s\n", x.name, d.Action, d.ID, it.text)
		if d.Action != "new" && d.Action != "retire" {
			settled[d.Item] = d.ID
		}
		switch d.Action {
		case "new":
			if body == "" {
				body = it.text
			}
			id := x.cfg.Kind + ":" + sum(x.name + "\x00" + it.from[0].ID + "\x00" + body)[:12]
			x.put(id, body, it, vec)
			settled[d.Item] = id
			st.Added++
		case "update":
			if body == "" {
				continue
			}
			if body != it.text {
				if again, err := x.embed(ctx, []string{body}); err == nil {
					vec = again[0]
				}
			}
			x.put(d.ID, body, it, vec)
			st.Updated++
		case "same":
			x.put(d.ID, text(x.state.Nodes[d.ID]), it, x.vector[d.ID])
			st.Same++
		case "merge":
			if body == "" {
				continue
			}
			st.Merged += x.absorb(d.ID, d.With, known)
			if again, err := x.embed(ctx, []string{body}); err == nil {
				vec = again[0]
			}
			x.put(d.ID, body, it, vec)
		case "retire":
			x.s.Drop(d.ID)
			delete(x.vector, d.ID)
			st.Retired++
		}
		for _, r := range d.Against {
			if id := x.itemID(r.ID); id != d.ID || d.Action == "new" {
				x.revise(ctx, r, it.from[len(it.from)-1], st)
			}
		}
	}
	return nil
}

// foreign is how many items of the bank's other enrichments are shown beside
// each new item, for it to speak against.
const foreign = 3

// others are the items of the bank's other enrichments nearest to the vectors.
func (x *run) others(vecs [][]float32) []*index.Node {
	var all []*index.Node
	for _, n := range x.state.Nodes {
		if index.Enrichment(n) != "" && n.Connector != x.owner && x.vector[n.ID] != nil {
			all = append(all, n)
		}
	}
	picked := map[string]bool{}
	var out []*index.Node
	for _, v := range vecs {
		sort.Slice(all, func(i, j int) bool {
			a, b := dot(v, x.vector[all[i].ID]), dot(v, x.vector[all[j].ID])
			if a != b {
				return a > b
			}
			return all[i].ID < all[j].ID
		})
		for _, n := range all[:min(len(all), foreign)] {
			if !picked[n.ID] && len(out) < shown/2 {
				picked[n.ID] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// revision is an item as it stands once a later statement has spoken against a
// part of it; with no text, nothing of it is left.
type revision struct{ ID, Text string }

// itemID is the id of an item of any enrichment as the model copied it.
func (x *run) itemID(id string) string {
	hex := idHex.FindString(id)
	if hex == "" {
		return ""
	}
	for _, n := range x.state.Nodes {
		if index.Enrichment(n) != "" && strings.HasSuffix(n.ID, ":"+hex) {
			return n.ID
		}
	}
	return ""
}

// revise rewrites the item a later statement spoke against, of this enrichment
// or another, or drops it. The item stays its enrichment's: the node that
// spoke against it, when there is one, is one more of the nodes behind it, so
// the item is as late as the correction.
func (x *run) revise(ctx context.Context, r revision, by *index.Node, st *Stats) {
	old := x.state.Nodes[x.itemID(r.ID)]
	body := strings.TrimSpace(r.Text)
	if old == nil || body == text(old) {
		return
	}
	if body == "" {
		fmt.Fprintf(x.s.Log(), "enrich %s: against %s, retired: %s\n", x.name, old.ID, text(old))
		x.s.Drop(old.ID)
		delete(x.vector, old.ID)
		st.Retired++
		return
	}
	fmt.Fprintf(x.s.Log(), "enrich %s: against %s: %s\n", x.name, old.ID, body)
	n := index.NewTextNode(old.ID, old.Kind, old.Connector, body, old.Attrs)
	n.At = old.At
	if by != nil {
		if by.At.After(n.At) {
			n.At = by.At
		}
		x.s.PutEdge(&index.Edge{From: by.ID, To: old.ID, Kind: EdgeStates, Weight: 1, Connector: old.Connector})
	}
	x.s.Put(n)
	if vecs, err := x.embed(ctx, []string{body}); err == nil {
		x.vector[old.ID] = vecs[0]
	}
	st.Updated++
}

// absorb moves the passages behind the listed kept items onto the one that
// stays and removes them; it returns how many went.
func (x *run) absorb(keep string, others []string, known map[string]bool) int {
	gone := 0
	for _, other := range others {
		if hex := idHex.FindString(other); hex != "" {
			other = x.cfg.Kind + ":" + hex
		}
		if other == keep || !known[other] || x.state.Nodes[other] == nil {
			continue
		}
		for _, e := range x.state.Edges {
			if e.To == other && e.Connector == x.owner {
				x.s.PutEdge(&index.Edge{From: e.From, To: keep, Kind: e.Kind, Weight: e.Weight, Connector: x.owner})
			}
		}
		fmt.Fprintf(x.s.Log(), "enrich %s:   with %s: %s\n", x.name, other, text(x.state.Nodes[other]))
		if x.state.Nodes[other].At.After(x.state.Nodes[keep].At) {
			x.state.Nodes[keep].At = x.state.Nodes[other].At
		}
		x.s.Drop(other)
		delete(x.vector, other)
		delete(known, other)
		gone++
	}
	return gone
}

const consolidateSchema = `{"type":"object","properties":{"merges":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"with":{"type":"array","items":{"type":"string"}},"text":{"type":"string"}},"required":["id","with","text"],"additionalProperties":false}}},"required":["merges"],"additionalProperties":false}`

const reviseSchema = `{"type":"object","properties":{"merges":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"with":{"type":"array","items":{"type":"string"}},"text":{"type":"string"}},"required":["id","with","text"],"additionalProperties":false}},"revisions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"}},"required":["id","text"],"additionalProperties":false}}},"required":["merges","revisions"],"additionalProperties":false}`

// looked is the reading an item was last looked at under, beside its hash: a
// change of what the pass looks for has it look at every item once more.
const looked = " c2"

const consolidateSystem = `You are given items kept for a project, each with its id: one item and the kept items nearest to it. Find the items among them that are about one matter, so that one item can say what all of them say.

For each such set give in "id" the item that stays, in "with" the ids of the items that go into it, and in "text" the one item that stands for them all: one to three sentences, complete in themselves. Nothing any of them constrains may be lost. Merge items about one matter, and never items that merely sit near each other or belong to one area; two items that constrain different things stay two. Most sets of items hold nothing to merge: answer with an empty list then.

Each item has the date it was last stated. A later item may overturn a part of an earlier one, or all of it: the earlier says several things, and the later cancels or replaces one of them. Give each item so overturned in "revisions": its id, and in "text" the item without what no longer holds, everything else of it kept word for word — or "" when nothing of it is left. Only a direct contradiction counts: an item that is narrower, broader or about something else is left as it is. Items under "Items of other readings" are never merged; they may be revised, and may be what overturns.

Answer with JSON: {"merges": [{"id": "…", "with": ["…"], "text": "…"}], "revisions": [{"id": "…", "text": "…"}]}`

// pressedSystem is the pass over an enrichment that keeps more than its ratio.
const pressedSystem = `You are given items kept for a project, each with its id: one item and the kept items nearest to it. The project keeps more items than it can use, and they are to become fewer and broader.

Group the items that come under one principle or govern one part of the system, and write each group as one item: what the principle is, then everything the grouped items require, each constraint kept with its particulars — names, paths, values — and with the condition it holds under: when, for whom, in which case it applies. A constraint that held for one case must not read as holding for all. Up to six sentences. For each group give in "id" the item that stays, in "with" the ids of the items that go into it, and in "text" the broader item. Items that share no principle stay apart: do not join unrelated rules into a list.

Answer with JSON: {"merges": [{"id": "…", "with": ["…"], "text": "…"}]}`

// consolidate is the same reading turned on the items themselves: each kept
// item that has not been looked at as it stands is shown with the kept items
// nearest to it, and the model says which of them are one.
//
// Pressed — the enrichment is over its ratio — it goes over every item again,
// each shown once in a pass, and merges what comes under one principle into a
// broader item: the items get fewer and longer. An item already at its length
// is left alone, so a pressed enrichment ends over its ratio rather than in a
// few items that say everything.
func (x *run) consolidate(ctx context.Context, st *Stats, pressed bool) error {
	items := x.kept()
	byTime(items)
	system := consolidateSystem
	if pressed {
		system = pressedSystem
		fmt.Fprintf(x.s.Log(), "enrich %s: %d items kept, the ratio asks for %d: pressing them together\n", x.name, len(items), x.cap())
	}
	full := func(n *index.Node) bool { return pressed && utf8.RuneCountInString(text(n)) > itemLimit }
	taken := map[string]bool{}
	for _, seed := range items {
		switch {
		case x.state.Nodes[seed.ID] == nil:
			continue
		case pressed && (taken[seed.ID] || full(seed) || x.over() <= 1):
			continue
		case !pressed && x.state.Enriched[x.key(seed.ID)] == seed.Hash+looked:
			continue
		}
		type scored struct {
			n *index.Node
			d float64
		}
		var near []scored
		for _, n := range x.kept() {
			if n.ID != seed.ID && !taken[n.ID] && !full(n) {
				near = append(near, scored{n, dot(x.vector[seed.ID], x.vector[n.ID])})
			}
		}
		sort.Slice(near, func(i, j int) bool {
			if near[i].d != near[j].d {
				return near[i].d > near[j].d
			}
			return near[i].n.ID < near[j].n.ID
		})
		near = near[:min(len(near), x.cfg.Neighbours)]
		x.state.Enriched[x.key(seed.ID)] = seed.Hash + looked
		var others []*index.Node
		if !pressed {
			others = x.others([][]float32{x.vector[seed.ID]})
		}
		if len(near)+len(others) == 0 {
			continue
		}
		known := map[string]bool{seed.ID: true}
		taken[seed.ID] = true
		var b strings.Builder
		line := func(n *index.Node) {
			fmt.Fprintf(&b, "- %s · %s: %s\n", n.ID, n.At.Format(time.DateOnly), text(n))
		}
		line(seed)
		for _, s := range near {
			known[s.n.ID] = true
			taken[s.n.ID] = true
			line(s.n)
		}
		if len(others) > 0 {
			b.WriteString("\nItems of other readings:\n")
			for _, n := range others {
				line(n)
			}
		}
		var out struct {
			Merges []struct {
				ID, Text string
				With     []string
			}
			Revisions []revision
		}
		schema := consolidateSchema
		if !pressed {
			schema = reviseSchema
		}
		if err := x.complete(ctx, system, b.String(), schema, &out); err != nil {
			if soft(err) {
				continue
			}
			return err
		}
		for _, m := range out.Merges {
			if hex := idHex.FindString(m.ID); hex != "" {
				m.ID = x.cfg.Kind + ":" + hex
			}
			body := strings.TrimSpace(m.Text)
			if !known[m.ID] || x.state.Nodes[m.ID] == nil || body == "" {
				continue
			}
			fmt.Fprintf(x.s.Log(), "enrich %s: merge %s: %s\n", x.name, m.ID, body)
			gone := x.absorb(m.ID, m.With, known)
			if gone == 0 {
				continue
			}
			st.Merged += gone
			keep := x.state.Nodes[m.ID]
			n := index.NewTextNode(m.ID, keep.Kind, x.owner, body, keep.Attrs)
			n.At = keep.At
			x.s.Put(n)
			if vecs, err := x.embed(ctx, []string{body}); err == nil {
				x.vector[m.ID] = vecs[0]
			}
		}
		for _, r := range out.Revisions {
			x.revise(ctx, r, nil, st)
		}
	}
	return x.s.SaveState()
}

// put writes the item as it stands now with the edges of the nodes that
// support it. It is dated by the latest of them, whose repo, project and
// branch come along as attrs beside the item's own fields and its type, the
// enrichment's name.
func (x *run) put(id, body string, it item, vec []float32) {
	latest := it.from[len(it.from)-1]
	attrs := map[string]string{}
	if old := x.state.Nodes[id]; old != nil {
		for k, v := range old.Attrs {
			attrs[k] = v
		}
		if old.At.After(latest.At) {
			latest = old
		}
	}
	for _, k := range []string{"repo", "project", "branch"} {
		if v := it.from[len(it.from)-1].Attrs[k]; v != "" {
			attrs[k] = v
		}
	}
	for k, v := range it.fields {
		attrs[k] = v
	}
	attrs["type"] = x.name
	n := index.NewTextNode(id, x.cfg.Kind, x.owner, body, attrs)
	n.At = latest.At
	x.s.Put(n)
	x.vector[id] = vec
	for _, m := range it.from {
		x.s.PutEdge(&index.Edge{From: m.ID, To: id, Kind: EdgeStates, Weight: 1, Connector: x.owner})
	}
}

// embed is the vectors of the texts under the bank's embedder, as documents.
// They are what a kept item is found by before the bank has embedded its node.
func (x *run) embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, tokens, err := x.s.Embedder().Embed(ctx, texts)
	x.s.Bank().RecordUsage(usage.Embed, x.s.Bank().Embedder.String(), tokens)
	if err != nil {
		return nil, err
	}
	for i := range vecs {
		vecs[i] = unit(vecs[i])
	}
	return vecs, nil
}

var errInvalidAnswer = errors.New("the answer is not the JSON asked for")

// soft is an error that is one neighbourhood's and not the run's.
func soft(err error) bool { return errors.Is(err, errInvalidAnswer) || errors.Is(err, llm.ErrNoAnswer) }

func (x *run) complete(ctx context.Context, system, user, schema string, out any) error {
	var parseErr error
	for range 2 {
		raw, tokens, err := x.client.Complete(ctx, system, user, json.RawMessage(schema))
		x.s.Bank().RecordUsage(usage.Enrich, x.client.ID(), tokens)
		if err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "```") {
			if nl := strings.IndexByte(raw, '\n'); nl >= 0 {
				raw = raw[nl+1:]
			}
			raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "```"))
		}
		if parseErr = json.Unmarshal([]byte(raw), out); parseErr == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: %v", errInvalidAnswer, parseErr)
}

// text is a node's text as its chunks hold it.
func text(n *index.Node) string {
	if n == nil {
		return ""
	}
	parts := make([]string, len(n.Chunks))
	for i, c := range n.Chunks {
		parts[i] = c.Text
	}
	return strings.Join(parts, "\n\n")
}

// clip keeps the head and the end of a long text: a document states its
// purpose first, and a turn reaches its conclusion last.
func clip(s string, runes int) string {
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	head := runes / 3
	return string(r[:head]) + "\n…\n" + string(r[len(r)-(runes-head):])
}

func tail(s string, runes int) string {
	if r := []rune(s); len(r) > runes {
		return "…" + string(r[len(r)-runes:])
	}
	return s
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// SetStatus is how far one enrichment has got over the bank.
type SetStatus struct {
	Name string `json:"name"`
	// Items are the nodes it keeps; Read and Pending split the nodes it
	// selects by whether it has a record of them.
	Items   int `json:"items"`
	Read    int `json:"read"`
	Pending int `json:"pending"`
}

// Status is every enrichment of the bank, by name. A node compacted away is in
// neither count.
func Status(b *bank.Bank, state *index.State) []SetStatus {
	out := make([]SetStatus, 0, len(b.Enrich.Sets))
	for name, e := range b.Enrich.Sets {
		st, selects := SetStatus{Name: name}, e.Resolved().Selector()
		for _, n := range state.Nodes {
			switch {
			case index.Enrichment(n) == name:
				st.Items++
			case !selects(n.Kind, n.ID):
			case state.Enriched[name+"\x00"+n.ID] != "":
				st.Read++
			default:
				st.Pending++
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Read says every enrichment of the bank that selects the node has been over
// it: what compaction asks before the node goes.
func Read(b *bank.Bank, state *index.State, n *index.Node) bool {
	for name, e := range b.Enrich.Sets {
		if e.Missing() == "" && e.Selects(n.Kind, n.ID) && state.Enriched[name+"\x00"+n.ID] == "" {
			return false
		}
	}
	return true
}
