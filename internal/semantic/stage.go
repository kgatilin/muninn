package semantic

import (
	"context"
	"fmt"
	"sort"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/llm"
)

// saveEvery is how many text nodes are read between two saves of the graph:
// what an interrupted run keeps.
const saveEvery = 512

// Stats is what one run of the stage did.
type Stats struct {
	Pending, Processed       int
	Accepted, Revised, Empty int
	Rejected, Dropped        int
	Swept, Embedded          int
	// Merged are the entity pairs the merge pass put together, Described the
	// entities it wrote a paragraph for.
	Merged, Described int
}

// Stage is the semantic stage as index.Options.After takes it, or nil for a
// bank whose method is off.
func Stage(b *bank.Bank) func(context.Context, *index.Session) error {
	if b.Semantic.Resolved().Method == bank.SemanticOff {
		return nil
	}
	return func(ctx context.Context, s *index.Session) error {
		_, err := Run(ctx, s, nil)
		return err
	}
}

// NewExtractor is the bank's configured extractor. The llm method's client is
// made here, so a hosted provider's missing key stops the stage before it
// reads a node.
func NewExtractor(s *index.Session) (Extractor, error) {
	cfg := s.Bank().Semantic.Resolved()
	switch cfg.Method {
	case bank.SemanticLLM:
		spec, err := llm.ParseSpec(cfg.Model)
		if err != nil {
			return nil, fmt.Errorf("semantic.model: %w", err)
		}
		spec.Endpoint = cfg.Endpoint
		client, err := llm.New(spec)
		if err != nil {
			return nil, fmt.Errorf("semantic stage: %w", err)
		}
		return NewLLM(s, client), nil
	}
	return nil, fmt.Errorf("semantic.method %q has no extractor", cfg.Method)
}

// signature is what State.Semantic holds for a node that has been read.
func signature(n *index.Node, ex Extractor) string { return n.Hash + " " + ex.ID() }

// Reads says whether the stage reads the node under the bank's settings: a
// text node of one of semantic.kinds.
func Reads(cfg bank.Semantic, n *index.Node) bool { return IsText(n) && cfg.Reads(n.Kind) }

// Pending are the text nodes the stage reads and the extractor has not read as
// they are now: newest first by their time, then the undated, each in id
// order. A budget that binds is then spent on recent material.
func Pending(state *index.State, cfg bank.Semantic, extractorID string) []string {
	var ids []string
	for id, n := range state.Nodes {
		if Reads(cfg, n) && state.Semantic[id] != n.Hash+" "+extractorID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := state.Nodes[ids[i]].At, state.Nodes[ids[j]].At
		if !a.Equal(b) {
			return a.After(b)
		}
		return ids[i] < ids[j]
	})
	return ids
}

// Run reads every pending text node: asks the extractor for a patch, proposes
// it, and on rejection hands the violations back for another, for the bank's
// number of rounds. What is still rejected is dropped and recorded; the node
// keeps its words and its structural edges. It runs under the session's lock,
// after the connectors have committed, and commits again at the end so that
// the new entities are chunked, embedded and indexed like any node. ex nil
// takes the bank's extractor.
func Run(ctx context.Context, s *index.Session, ex Extractor) (Stats, error) {
	checks, err := BuildChecks(s.Bank().Semantic)
	if err != nil {
		return Stats{}, err
	}
	return run(ctx, s, ex, checks)
}

func run(ctx context.Context, s *index.Session, ex Extractor, checks []Check) (Stats, error) {
	var st Stats
	cfg := s.Bank().Semantic.Resolved()
	state := s.State()
	if state.Semantic == nil {
		state.Semantic = map[string]string{}
	}
	if ex == nil {
		var err error
		if ex, err = NewExtractor(s); err != nil {
			return st, err
		}
	}
	journal, err := OpenJournal(s.Bank().IndexDir())
	if err != nil {
		return st, err
	}
	defer journal.Close()

	layer := NewLayer(state)
	layer.countTexts(func(n *index.Node) bool { return Reads(cfg, n) })
	gate := &Gate{Layer: layer, Checks: checks, Hops: cfg.Hops, RegionCap: cfg.RegionCap}
	for id := range state.Semantic {
		if state.Nodes[id] == nil {
			delete(state.Semantic, id)
		}
	}
	pending := Pending(state, cfg, ex.ID())
	st.Pending = len(pending)
	log := s.Log()

	nodes := make([]*index.Node, 0, len(pending))
	for _, id := range pending {
		nodes = append(nodes, state.Nodes[id])
	}
	var runErr error
	if p, ok := ex.(Preparer); ok {
		runErr = p.Prepare(ctx, nodes, gate.Layer)
	}
	for _, n := range nodes {
		if runErr != nil {
			break
		}
		if runErr = ctx.Err(); runErr != nil {
			break
		}
		// A node read before and changed since says something else now.
		if _, again := state.Semantic[n.ID]; again {
			layer.DropEdgesOf(n.ID)
		}
		if runErr = readNode(ctx, gate, ex, journal, n, cfg.Rounds, &st); runErr != nil {
			break
		}
		state.Semantic[n.ID] = signature(n, ex)
		st.Processed++
		if st.Processed%saveEvery == 0 {
			if runErr = journal.Flush(); runErr == nil {
				runErr = s.SaveState()
			}
			if runErr == nil {
				fmt.Fprintf(log, "semantic: %d/%d text nodes, %d patches accepted, %d dropped\n", st.Processed, st.Pending, st.Accepted, st.Dropped)
			}
		}
	}

	// A run that stopped has not read every passage that chose a name, so an
	// entity short of its votes may yet get them: only the bare ones go.
	if runErr != nil {
		st.Swept = layer.Sweep(1)
	} else {
		st.Swept = layer.Sweep(cfg.Votes)
	}
	if runErr != nil {
		// What was done stays: the graph is saved, and the next commit embeds
		// and indexes the entities this one did not get to.
		if err := s.SaveState(); err != nil {
			return st, err
		}
		return st, fmt.Errorf("semantic stage stopped after %d of %d text nodes: %w", st.Processed, st.Pending, runErr)
	}
	if st.Processed > 0 || st.Swept > 0 {
		if st.Embedded, err = s.Commit(ctx); err != nil {
			return st, err
		}
	}
	// The pass runs on a bank with nothing new to read as well: a layer built
	// before entities had descriptions gets them here.
	if f, ok := ex.(Finisher); ok {
		if err := finish(ctx, s, gate, f, journal, &st); err != nil {
			return st, err
		}
	}
	c := layer.Counts()
	if st.Pending == 0 && st.Swept == 0 && st.Described == 0 {
		fmt.Fprintf(log, "semantic %s: nothing new; layer %d entities, %d topics, %d edges\n", ex.ID(), c.Entities, c.Topics, c.Edges)
		return st, nil
	}
	fmt.Fprintf(log, "semantic %s: %d text nodes read, %d patches accepted (%d after revision), %d rejections, %d dropped, %d with no candidates, %d entities merged, %d described; layer %d entities, %d topics, %d edges\n",
		ex.ID(), st.Processed, st.Accepted, st.Revised, st.Rejected, st.Dropped, st.Empty, st.Merged, st.Described, c.Entities, c.Topics, c.Edges)
	return st, nil
}

// finish runs the extractor's pass over the whole layer. It comes after the
// commit, so the entities this run made have their vectors, and its patches
// pass the same gate; what it changed is committed again.
func finish(ctx context.Context, s *index.Session, gate *Gate, f Finisher, journal *Journal, st *Stats) error {
	patches, err := f.Finish(ctx, gate.Layer)
	for _, p := range patches {
		ev, perr := gate.Propose(p)
		ev.Round = 1
		switch {
		case perr != nil:
			ev.Decision = "dropped"
			st.Dropped++
		case ev.Decision == Accept && len(p.Ops) > 0 && p.Ops[0].Kind == OpDescribe:
			st.Described++
		case ev.Decision == Accept:
			st.Merged++
		default:
			ev.Decision = "dropped"
			st.Rejected++
			st.Dropped++
		}
		if jerr := journal.Append(ev); jerr != nil {
			return jerr
		}
	}
	if st.Merged > 0 || st.Described > 0 {
		if _, cerr := s.Commit(ctx); cerr != nil {
			return cerr
		}
	} else if serr := s.SaveState(); serr != nil {
		// What the pass asked about and left as it was is not asked about again.
		return serr
	}
	if err != nil {
		return fmt.Errorf("semantic stage: the pass over the layer stopped: %w", err)
	}
	return nil
}

func readNode(ctx context.Context, gate *Gate, ex Extractor, journal *Journal, n *index.Node, rounds int, st *Stats) error {
	patch, err := ex.Extract(ctx, n, gate.Layer)
	if err != nil {
		return fmt.Errorf("%s: %w", n.ID, err)
	}
	if len(patch.Ops) == 0 {
		st.Empty++
		return nil
	}
	for round := 1; ; round++ {
		ev, err := gate.Propose(patch)
		ev.Node, ev.Round = n.ID, round
		if err != nil {
			// A patch that does not apply is the extractor's mistake, not the
			// graph's: recorded, dropped, and the run goes on.
			ev.Decision = "dropped"
			st.Dropped++
			return journal.Append(ev)
		}
		if ev.Decision == Accept {
			st.Accepted++
			if round > 1 {
				st.Revised++
			}
			return journal.Append(ev)
		}
		st.Rejected++
		next, ok := Patch{}, false
		if round < rounds {
			next, ok = ex.Revise(ctx, n, patch, ev.Violations(), round)
		}
		if !ok || len(next.Ops) == 0 {
			ev.Decision = "dropped"
			st.Dropped++
			return journal.Append(ev)
		}
		if err := journal.Append(ev); err != nil {
			return err
		}
		patch = next
	}
}

// Read says the stage has been over the node, or does not read it at all:
// what compaction asks before the node goes.
func Read(cfg bank.Semantic, state *index.State, n *index.Node) bool {
	cfg = cfg.Resolved()
	return cfg.Method == bank.SemanticOff || !Reads(cfg, n) || state.Semantic[n.ID] != ""
}
