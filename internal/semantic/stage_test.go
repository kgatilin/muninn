package semantic_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/internal/semantic"
	"github.com/kgatilin/muninn/stream"
)

// The connector is `cat <file>`, as in the index package's tests.
func newBank(t *testing.T, write func(w *stream.Writer), settings ...string) *bank.Bank {
	t.Helper()
	t.Setenv("MUNINN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	write(w)
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
	for _, kv := range append([]string{"embedder=noop", "semantic.model=ollama", "semantic.method=llm", "semantic.votes=2", "search.seeds=1"}, settings...) {
		k, v, _ := strings.Cut(kv, "=")
		if _, err := b.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	b.Connectors = []bank.Connector{{Name: "src", Command: []string{"cat", file}}}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	return b
}

// phrases is an extractor with no model, for the tests of the stage itself: a
// node mentions each of the phrases its text holds. Its id is the bank's
// extractor's, so Status and Pending read its record as the bank's own.
type phrases struct {
	id    string
	known []string
}

func (p phrases) ID() string { return p.id }

func (p phrases) Extract(_ context.Context, n *index.Node, _ semantic.View) (semantic.Patch, error) {
	var text strings.Builder
	for _, c := range n.Chunks {
		text.WriteString(strings.ToLower(c.Text) + "\n")
	}
	var patch semantic.Patch
	for _, phrase := range p.known {
		if strings.Contains(text.String(), phrase) {
			patch.Ops = append(patch.Ops, semantic.AddEntity(phrase), semantic.AddMention(n.ID, semantic.EntityID(phrase), 1))
		}
	}
	return patch, nil
}

func (phrases) Revise(context.Context, *index.Node, semantic.Patch, []semantic.Result, int) (semantic.Patch, bool) {
	return semantic.Patch{}, false
}

func runIndex(t *testing.T, b *bank.Bank) (semantic.Stats, *index.State) {
	t.Helper()
	var st semantic.Stats
	ex := phrases{id: semantic.ExtractorID(b.Semantic), known: []string{"journal stream", "sourdough"}}
	err := index.Run(context.Background(), b, index.Options{After: func(ctx context.Context, s *index.Session) (err error) {
		st, err = semantic.Run(ctx, s, ex)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := index.LoadState(b.IndexDir())
	if err != nil {
		t.Fatal(err)
	}
	return st, state
}

func threeNotes(w *stream.Writer) {
	w.Node(stream.Node{ID: "/n/ravens.md", Kind: "document", Text: "Huginn flies at dawn. The watcher appends to the journal stream."})
	w.Node(stream.Node{ID: "/n/fold.md", Kind: "document", Text: "A controller folds the journal stream. Sourdough is unrelated."})
	w.Node(stream.Node{ID: "/n/bread.md", Kind: "document", Text: "Sourdough wants patience."})
	w.Node(stream.Node{ID: "/n", Kind: "directory"})
	w.Edge(stream.Edge{From: "/n", To: "/n/bread.md", Kind: "contains"})
	w.Sweep()
}

func TestStageEndToEnd(t *testing.T) {
	// The directory's edges are out of the walk so that what reaches the second
	// note can only have come through an entity.
	b := newBank(t, threeNotes, "search.edge_weights.contains=0")
	st, state := runIndex(t, b)
	if st.Processed != 3 || st.Accepted == 0 {
		t.Fatalf("first run: %+v", st)
	}

	// Phrases two notes share are entities; a phrase one note holds is not.
	var entities []string
	for id, n := range state.Nodes {
		if n.Connector != index.SemanticConnector {
			continue
		}
		entities = append(entities, id)
		if n.Kind != semantic.KindEntity || len(n.Chunks) != 1 || n.Attrs["name"] == "" {
			t.Errorf("entity %s = %+v", id, n)
		}
		if strings.Contains(id, "huginn") || strings.Contains(id, "patience") {
			t.Errorf("%s occurs in one note and became an entity", id)
		}
	}
	shared := func(a, b string) bool {
		for _, e := range entities {
			if state.Edges[(index.Edge{From: a, To: e, Kind: semantic.EdgeMentions}).Key()] != nil &&
				state.Edges[(index.Edge{From: b, To: e, Kind: semantic.EdgeMentions}).Key()] != nil {
				return true
			}
		}
		return false
	}
	if !shared("/n/ravens.md", "/n/fold.md") || !shared("/n/fold.md", "/n/bread.md") {
		t.Fatalf("the notes are not tied through entities: %v", entities)
	}

	// An entity is searchable through the ordinary channels.
	hits, _, err := search.Run(context.Background(), b, "sourdough", search.Options{K: 10, Kind: semantic.KindEntity, NoGraph: true})
	if err != nil || len(hits) == 0 || hits[0].Node.ID != "entity:sourdough" {
		t.Fatalf("searching for the entity: %v %+v", err, hits)
	}

	// One seed, the note about ravens. The walk reaches the note about folding
	// through the entity they share, and the bread beyond it.
	reached := func() map[string]float64 {
		hits, _, err := search.Run(context.Background(), b, "huginn dawn", search.Options{K: 20})
		if err != nil || len(hits) == 0 || hits[0].Node.ID != "/n/ravens.md" || !hits[0].Seed {
			t.Fatalf("the seed: %v %+v", err, hits)
		}
		mass := map[string]float64{}
		for _, h := range hits {
			if !h.Seed {
				mass[h.Node.ID] = h.Mass
			}
		}
		return mass
	}
	if mass := reached(); mass["/n/fold.md"] <= 0 || mass["/n/fold.md"] <= mass["/n/bread.md"] {
		t.Errorf("the walk through the layer: %v", mass)
	}

	// A second run reads nothing; a changed note is read again and its old
	// mentions go.
	if st, _ := runIndex(t, b); st.Pending != 0 || st.Processed != 0 {
		t.Errorf("an unchanged re-run: %+v", st)
	}
	records, err := semantic.ReadJournal(b.IndexDir(), 0, false)
	if err != nil || len(records) == 0 || records[0].Node == "" || len(records[0].Checks) == 0 {
		t.Errorf("the evidence log: %v %+v", err, records)
	}

	// Reset takes the layer and nothing else.
	s, err := index.Open(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	nodes, edges := semantic.Reset(s.State())
	if _, err := s.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	state, _ = index.LoadState(b.IndexDir())
	if nodes != len(entities) || edges == 0 || len(state.Nodes) != 4 || len(state.Edges) != 1 || len(state.Semantic) != 0 {
		t.Errorf("reset took %d nodes, %d edges; left %d nodes, %d edges, %d read", nodes, edges, len(state.Nodes), len(state.Edges), len(state.Semantic))
	}
	if mass := reached(); mass["/n/fold.md"] != 0 {
		t.Errorf("with the layer gone the walk still reaches the second note: %v", mass)
	}
	if st, _ := runIndex(t, b); st.Processed != 3 {
		t.Errorf("after a reset: %+v", st)
	}
}

func TestAChangedNoteIsReadAgain(t *testing.T) {
	b := newBank(t, threeNotes)
	runIndex(t, b)
	// The note about bread stops being about bread.
	file := b.Connectors[0].Command[1]
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	w.Node(stream.Node{ID: "/n/ravens.md", Kind: "document", Text: "Huginn flies at dawn. The watcher appends to the journal stream."})
	w.Node(stream.Node{ID: "/n/fold.md", Kind: "document", Text: "A controller folds the journal stream. Sourdough is unrelated."})
	w.Node(stream.Node{ID: "/n/bread.md", Kind: "document", Text: "Rye wants patience."})
	w.Sweep()
	w.Flush()
	os.WriteFile(file, buf.Bytes(), 0o644)

	st, state := runIndex(t, b)
	if st.Processed != 1 {
		t.Errorf("after one note changed: %+v", st)
	}
	// One note still mentions the entity, which is fewer than the bank's
	// votes: it ties nothing together any more, and goes.
	if state.Nodes["entity:sourdough"] != nil || st.Swept != 1 {
		t.Errorf("an entity one note mentions stayed: %+v", st)
	}
	for _, e := range state.Edges {
		if e.From == "/n/bread.md" && e.Connector == index.SemanticConnector {
			t.Errorf("a stale mention: %+v", e)
		}
	}
}

// A connector cannot write into the layer.
func TestAConnectorCannotEmitAnEntity(t *testing.T) {
	b := newBank(t, threeNotes)
	runIndex(t, b)
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	w.Node(stream.Node{ID: "entity:sourdough", Kind: "document", Text: "mine now"})
	w.Flush()
	os.WriteFile(b.Connectors[0].Command[1], buf.Bytes(), 0o644)
	if err := index.Run(context.Background(), b, index.Options{}); err == nil || !strings.Contains(err.Error(), "semantic layer") {
		t.Errorf("err = %v", err)
	}
}

// semantic.kinds limits what the stage reads, and what the budget counts; the
// pending nodes come newest first, the undated last.
func TestKindsAndOrder(t *testing.T) {
	at := func(day int) *time.Time { d := time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC); return &d }
	b := newBank(t, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "turn:1", Kind: "turn", At: at(1), Text: "The journal stream is folded."})
		w.Node(stream.Node{ID: "turn:2", Kind: "turn", At: at(3), Text: "A watcher appends to the journal stream."})
		w.Node(stream.Node{ID: "turn:0", Kind: "turn", Text: "Undated, about the journal stream."})
		w.Node(stream.Node{ID: "/a.go", Kind: "document", Text: "package a // the journal stream"})
	}, "semantic.kinds=turn")
	st, state := runIndex(t, b)
	if st.Processed != 3 || state.Semantic["/a.go"] != "" {
		t.Errorf("read %+v, the document among them: %q", st, state.Semantic["/a.go"])
	}
	for _, e := range state.Edges {
		if e.From == "/a.go" {
			t.Errorf("a kind left out has an edge of the layer: %+v", e)
		}
	}
	if got := semantic.Pending(state, bank.Semantic{Kinds: []string{"turn"}}, "another/1"); !reflect.DeepEqual(got, []string{"turn:2", "turn:1", "turn:0"}) {
		t.Errorf("pending = %v", got)
	}
	if st := semantic.Status(state, b.Semantic); st.Counts.Texts != 3 || st.Pending != 0 || st.Processed != 3 {
		t.Errorf("status = %+v", st)
	}
}
