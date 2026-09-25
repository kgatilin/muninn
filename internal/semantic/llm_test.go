package semantic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/semantic"
	"github.com/kgatilin/muninn/internal/usage"
	"github.com/kgatilin/muninn/stream"
)

// scripted is a chat client whose answers a test writes: one function per
// prompt — extraction, revision, merge — told apart by the system prompt.
type scripted struct {
	mu                               sync.Mutex
	extract, revise, merge, describe func(user string) string
	users                            []string
}

func (*scripted) ID() string { return "fake:model" }

func (s *scripted) Complete(_ context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !json.Valid(schema) {
		return "", usage.Tokens{}, nil
	}
	s.users = append(s.users, user)
	switch {
	case strings.HasPrefix(system, "You name"):
		return s.extract(user), usage.Tokens{In: 10, Out: 2, Requests: 1}, nil
	case strings.HasPrefix(system, "A patch you proposed"):
		return s.revise(user), usage.Tokens{In: 30, Out: 3, Requests: 1}, nil
	case strings.HasPrefix(system, "You write the description"):
		if s.describe == nil {
			return `{"summary":"A thing the notes are about."}`, usage.Tokens{In: 20, Out: 5, Requests: 1}, nil
		}
		return s.describe(user), usage.Tokens{In: 20, Out: 5, Requests: 1}, nil
	case s.merge == nil:
		return `{"same":[]}`, usage.Tokens{}, nil
	default:
		return s.merge(user), usage.Tokens{}, nil
	}
}

func runLLM(t *testing.T, b *bank.Bank, client *scripted, before func(*index.Session)) (semantic.Stats, *index.State) {
	t.Helper()
	var st semantic.Stats
	err := index.Run(context.Background(), b, index.Options{After: func(ctx context.Context, s *index.Session) (err error) {
		if before != nil {
			before(s)
		}
		st, err = semantic.Run(ctx, s, semantic.NewLLM(s, client))
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

func mentions(state *index.State, from, to string) bool {
	return state.Edges[(index.Edge{From: from, To: to, Kind: semantic.EdgeMentions}).Key()] != nil
}

func passage(user string) string {
	line, _, _ := strings.Cut(user, "\n")
	return strings.TrimPrefix(line, "Passage: ")
}

var llmSettings = []string{"semantic.model=ollama", "semantic.method=llm", "semantic.concurrency=1"}

// singletonGate rejects the second entity of a bank when one note mentions it:
// the budget no longer can, since the run spends it before the gate sees a patch.
var singletonGate = []string{"semantic.checks.singleton_share.enabled=true", "semantic.checks.singleton_share.min_entities=1", "semantic.checks.singleton_share.ceiling=0.4"}

// oneVoice mints a name a single passage chose: the tests of what happens to
// a patch after that do not need a second passage for every name.
var oneVoice = append(llmSettings[:len(llmSettings):len(llmSettings)], "semantic.votes=1")

// An entity is minted for the name two passages of the run chose, however they
// wrote it, and not for a name one passage chose. A later run is shown the
// entity and reuses it by id, which needs no second voice.
func TestLLMMintsWhatTwoPassagesChose(t *testing.T) {
	b := newBank(t, threeNotes, llmSettings...)
	client := &scripted{extract: func(user string) string {
		if !strings.Contains(user, "none yet") {
			t.Errorf("candidates in an empty layer:\n%s", user)
		}
		switch passage(user) {
		case "/n/fold.md":
			return `{"mentions":[{"id":"","name":"Journal  Stream","confidence":0.9},{"id":"","name":"controller","confidence":0.8}]}`
		case "/n/ravens.md":
			return `{"mentions":[{"id":"","name":"journal stream","confidence":0.8},{"id":"","name":"Huginn","confidence":0.7}]}`
		}
		return `{"mentions":[]}`
	}}
	client.describe = func(user string) string {
		for _, want := range []string{"Entity: journal stream", "Mentioned by 2 notes", "--- /n/fold.md\nA controller folds the journal stream.", "--- /n/ravens.md"} {
			if !strings.Contains(user, want) {
				t.Errorf("the evidence does not say %q:\n%s", want, user)
			}
		}
		return `{"summary":"The stream the watcher appends to\n and the controller folds."}`
	}
	st, state := runLLM(t, b, client, nil)
	if st.Processed != 3 || st.Accepted != 2 || st.Empty != 1 || st.Described != 1 {
		t.Fatalf("%+v", st)
	}
	if !mentions(state, "/n/fold.md", "entity:journal stream") || !mentions(state, "/n/ravens.md", "entity:journal stream") {
		t.Fatalf("the notes do not meet at the entity")
	}
	if state.Nodes["entity:huginn"] != nil || state.Nodes["entity:controller"] != nil {
		t.Fatalf("an entity for a name one passage chose")
	}
	// The entity is described from the notes that mention it, and the
	// paragraph is part of the text the bank indexes under it.
	e := state.Nodes["entity:journal stream"]
	if e.Attrs["summary"] != "The stream the watcher appends to and the controller folds." || len(e.Chunks) != 1 || !strings.HasSuffix(e.Chunks[0].Text, e.Attrs["summary"]) {
		t.Fatalf("entity: %+v", e)
	}
	// The run is resumable per node: a second one has nothing to read.
	asked := len(client.users)
	if st, _ := runLLM(t, b, client, nil); st.Pending != 0 || len(client.users) != asked {
		t.Fatalf("a second run read again: %+v, %d calls", st, len(client.users)-asked)
	}

	// A note that arrives later.
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	threeNotes(w)
	w.Node(stream.Node{ID: "/n/late.md", Kind: "document", Text: "The journal stream is compacted at night."})
	w.Sweep()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.Connectors[0].Command[1], buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	client.extract = func(user string) string {
		if passage(user) != "/n/late.md" || !strings.Contains(user, "entity:journal stream | journal stream | 2") {
			t.Errorf("the entity of the earlier run is not offered:\n%s", user)
		}
		return `{"mentions":[{"id":"entity:journal stream","name":"","confidence":0.8},{"id":"","name":"compaction","confidence":0.7}]}`
	}
	st, state = runLLM(t, b, client, nil)
	if st.Processed != 1 || !mentions(state, "/n/late.md", "entity:journal stream") || state.Nodes["entity:compaction"] != nil {
		t.Fatalf("%+v", st)
	}
}

// An answer of the first pass that an earlier, stopped run kept is not asked
// for again: the model is asked about the one passage that has none.
func TestLLMKeepsTheFirstPassOfAStoppedRun(t *testing.T) {
	b := newBank(t, threeNotes, "semantic.method=off")
	asked := 0
	client := &scripted{extract: func(user string) string {
		asked++
		if passage(user) != "/n/bread.md" {
			t.Errorf("%s was asked about again", passage(user))
		}
		return `{"mentions":[{"id":"","name":"sourdough","confidence":0.9}]}`
	}}
	st, state := runLLM(t, b, client, func(s *index.Session) {
		id := semantic.NewLLM(s, client).ID()
		s.State().Named = map[string]string{
			"/n/ravens.md": s.State().Nodes["/n/ravens.md"].Hash + " " + id + "\n" + `[{"id":"","name":"journal stream","confidence":0.9}]`,
			"/n/fold.md":   s.State().Nodes["/n/fold.md"].Hash + " " + id + "\n" + `[{"id":"","name":"journal stream","confidence":0.9},{"id":"","name":"sourdough","confidence":0.8}]`,
			"/n/gone.md":   "whatever",
		}
	})
	if asked != 1 || st.Processed != 3 {
		t.Fatalf("asked %d times, stats %+v", asked, st)
	}
	for _, e := range [][2]string{{"/n/ravens.md", "entity:journal stream"}, {"/n/fold.md", "entity:journal stream"}, {"/n/fold.md", "entity:sourdough"}, {"/n/bread.md", "entity:sourdough"}} {
		if !mentions(state, e[0], e[1]) {
			t.Errorf("%s does not mention %s", e[0], e[1])
		}
	}
	if _, ok := state.Named["/n/gone.md"]; ok {
		t.Error("an answer for a node that is not pending was kept")
	}
}

func fourAgents(w *stream.Writer) {
	for _, n := range []string{"a", "b", "c"} {
		w.Node(stream.Node{ID: "/n/" + n + ".md", Kind: "document", Text: "The agent does " + n + "."})
	}
	w.Node(stream.Node{ID: "/n/d.md", Kind: "document", Text: "The agent runtime restarts the agent."})
	w.Sweep()
}

func TestLLMRevisesIntoAnIntermediate(t *testing.T) {
	// A second entity that one note mentions is over the singleton ceiling.
	b := newBank(t, fourAgents, append(oneVoice, singletonGate...)...)
	client := &scripted{
		extract: func(user string) string {
			if passage(user) == "/n/d.md" {
				return `{"mentions":[{"id":"entity:agent","name":"","confidence":0.9},{"id":"","name":"agent runtime","confidence":0.8}]}`
			}
			return `{"mentions":[{"id":"","name":"agent","confidence":0.9}]}`
		},
		revise: func(user string) string {
			for _, want := range []string{"singleton_share", "over the ceiling 0.4", "Entity entity:agent | agent | 3", "passage /n/a.md — The agent does a."} {
				if !strings.Contains(user, want) {
					t.Errorf("the rejection does not say %q:\n%s", want, user)
				}
			}
			return `{"action":"insert_intermediate","mentions":[{"id":"entity:agent","name":"","confidence":0.9}],"entity":"entity:agent","name":"Agent Runtime","nodes":["/n/a.md","/n/b.md"]}`
		},
	}
	st, state := runLLM(t, b, client, nil)
	if st.Accepted != 4 || st.Revised != 1 || st.Rejected != 1 || st.Dropped != 0 {
		t.Fatalf("%+v", st)
	}
	topic := "topic:agent runtime"
	if n := state.Nodes[topic]; n == nil || n.Kind != semantic.KindTopic {
		t.Fatalf("no topic: %+v", n)
	}
	for _, id := range []string{"/n/a.md", "/n/b.md", "/n/d.md"} {
		if !mentions(state, id, topic) || mentions(state, id, "entity:agent") {
			t.Errorf("%s is not under the topic", id)
		}
	}
	if !mentions(state, "/n/c.md", "entity:agent") || state.Nodes["entity:agent runtime"] != nil ||
		state.Edges[(index.Edge{From: topic, To: "entity:agent", Kind: semantic.EdgePartOf}).Key()] == nil {
		t.Error("the rest of the structure is not as revised")
	}

	// Every call of the model is a line of the usage log, under its purpose.
	home, _ := bank.Home()
	recs, err := usage.Read(home)
	if err != nil {
		t.Fatal(err)
	}
	lines, total, _ := usage.Summarize(recs, nil, "purpose")
	if len(lines) != 3 || lines[0].Key != usage.Describe || lines[1].Key != usage.Extract || lines[1].Calls != 4 || lines[2].Key != usage.Revise || lines[2].In != 30 ||
		total.Out != 16 || recs[0].Bank != b.Name || recs[0].Model != "fake:model" {
		t.Fatalf("usage %+v", lines)
	}
}

func TestLLMInvalidRevisionFallsBack(t *testing.T) {
	b := newBank(t, fourAgents, append(oneVoice, singletonGate...)...)
	client := &scripted{
		extract: func(user string) string {
			if passage(user) == "/n/d.md" {
				return `{"mentions":[{"id":"entity:agent","name":"","confidence":0.9},{"id":"","name":"agent runtime","confidence":0.8}]}`
			}
			return `{"mentions":[{"id":"","name":"agent","confidence":0.9}]}`
		},
		// A passage that does not mention the entity, under an entity that is
		// not there: not a patch.
		revise: func(string) string {
			return `{"action":"insert_intermediate","mentions":[],"entity":"entity:daemon","name":"x y","nodes":["/n/zzz.md"]}`
		},
	}
	st, state := runLLM(t, b, client, nil)
	// The fallback drops the weakest mention, the minted one, and that passes.
	if st.Processed != 4 || st.Accepted != 4 || st.Revised != 1 || !mentions(state, "/n/d.md", "entity:agent") || state.Nodes["entity:agent runtime"] != nil {
		t.Fatalf("%+v", st)
	}
}

func TestLLMInvalidOutputIsDropped(t *testing.T) {
	b := newBank(t, threeNotes, oneVoice...)
	client := &scripted{extract: func(user string) string {
		switch passage(user) {
		case "/n/bread.md":
			return "Sure! Here are the entities"
		case "/n/fold.md":
			// An id that is not in the graph, a name that is a number, a name
			// that is a sentence; one valid entry, in a code fence.
			return "```json\n" + `{"mentions":[{"id":"entity:nowhere","name":"","confidence":1},{"id":"","name":"2024","confidence":1},
				{"id":"","name":"a controller that folds the journal stream","confidence":1},{"id":"","name":"controller","confidence":0.5}]}` + "\n```"
		}
		return `{"mentions":[{"id":"/n/fold.md","name":"","confidence":1}]}`
	}}
	st, state := runLLM(t, b, client, nil)
	if st.Processed != 3 || st.Accepted != 1 || st.Empty != 2 {
		t.Fatalf("%+v", st)
	}
	var made []string
	for id, n := range state.Nodes {
		if semantic.IsSemantic(n) {
			made = append(made, id)
		}
	}
	if len(made) != 1 || made[0] != "entity:controller" {
		t.Fatalf("entities: %v", made)
	}
	// bread.md was asked twice: once more after the answer that did not parse.
	asked := 0
	for _, u := range client.users {
		if passage(u) == "/n/bread.md" {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("bread.md asked %d times", asked)
	}
}

// A plural the model names is the singular's entity when the bank holds the
// singular, and the entity keeps the plural as an alias.
func TestLLMFoldsAPlural(t *testing.T) {
	b := newBank(t, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "/n/a.md", Kind: "document", Text: "Worktrees are listed, and the status is shown."})
		w.Node(stream.Node{ID: "/n/b.md", Kind: "document", Text: "A worktree is removed."})
		w.Sweep()
	}, llmSettings...)
	names := map[string]string{"/n/a.md": "Worktrees", "/n/b.md": "worktree"}
	client := &scripted{extract: func(user string) string {
		return `{"mentions":[{"id":"","name":"` + names[passage(user)] + `","confidence":0.9},{"id":"","name":"status","confidence":0.5}]}`
	}}
	_, state := runLLM(t, b, client, nil)
	e := state.Nodes["entity:worktree"]
	if e == nil || e.Attrs["aliases"] != "worktrees" || state.Nodes["entity:worktrees"] != nil || state.Nodes["entity:status"] == nil {
		t.Fatalf("entity:worktree = %+v", e)
	}
	if !mentions(state, "/n/a.md", "entity:worktree") || !mentions(state, "/n/b.md", "entity:worktree") {
		t.Error("the two notes do not meet on the singular")
	}
}

func TestLLMMergePass(t *testing.T) {
	b := newBank(t, func(w *stream.Writer) {
		w.Node(stream.Node{ID: "/n/a.md", Kind: "document", Text: "The event log is appended to."})
		w.Node(stream.Node{ID: "/n/b.md", Kind: "document", Text: "The eventlog is replayed."})
		w.Node(stream.Node{ID: "/n/c.md", Kind: "document", Text: "The event bus delivers."})
		w.Sweep()
	}, oneVoice...)
	names := map[string]string{"/n/a.md": "event log", "/n/b.md": "eventlog", "/n/c.md": "event bus"}
	client := &scripted{
		extract: func(user string) string {
			return `{"mentions":[{"id":"","name":"` + names[passage(user)] + `","confidence":0.9}]}`
		},
		merge: func(user string) string {
			if strings.Count(user, "Pair ") != 3 {
				t.Errorf("pairs shown:\n%s", user)
			}
			var same []string
			for i, p := range strings.Split(user, "Pair ")[1:] {
				if strings.Contains(p, "entity:event log |") && strings.Contains(p, "entity:eventlog |") {
					same = append(same, `{"pair":`+string(rune('1'+i))+`,"keep":"entity:event log"}`)
				}
			}
			// And a pair that is not there, which is ignored.
			return `{"same":[` + strings.Join(append(same, `{"pair":9,"keep":"entity:event bus"}`), ",") + `]}`
		},
	}
	// The three names are one direction in the vector space, give or take.
	near := func(s *index.Session) {
		var hashes []string
		var vecs [][]float32
		for i, name := range []string{"event log", "eventlog", "event bus"} {
			v := make([]float32, 64)
			v[0], v[i+1] = 0.9644, 0.2645 // unit length, cosine 0.93: under the fold, over the merge
			hashes, vecs = append(hashes, index.ChunkHash(name)), append(vecs, v)
		}
		if err := s.Vectors().Append(hashes, vecs); err != nil {
			t.Fatal(err)
		}
	}
	st, state := runLLM(t, b, client, near)
	if st.Merged != 1 {
		t.Fatalf("%+v", st)
	}
	kept := state.Nodes["entity:event log"]
	if kept == nil || kept.Attrs["aliases"] != "eventlog" || state.Nodes["entity:eventlog"] != nil || state.Nodes["entity:event bus"] == nil {
		t.Fatalf("after the merge: %+v", kept)
	}
	if !mentions(state, "/n/a.md", "entity:event log") || !mentions(state, "/n/b.md", "entity:event log") || !mentions(state, "/n/c.md", "entity:event bus") {
		t.Fatal("the mentions did not follow the merge")
	}
}

func TestLLMMissingKeyStopsBeforeReading(t *testing.T) {
	b := newBank(t, threeNotes, "semantic.model=anthropic", "semantic.method=llm")
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := index.Run(context.Background(), b, index.Options{After: semantic.Stage(b)})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("index with no key: %v", err)
	}
	state, lerr := index.LoadState(b.IndexDir())
	if lerr != nil || len(state.Semantic) != 0 {
		t.Fatalf("nodes were read: %v %v", state.Semantic, lerr)
	}
	if got := semantic.Status(state, b.Semantic); got.Pending != 3 {
		t.Fatalf("pending = %d", got.Pending)
	}
}

// A name and its translation sit under the merge cosine. They are paired all
// the same, asked about once, and one entity when the model says so.
func TestLLMMergePassPairsATranslation(t *testing.T) {
	for _, same := range []bool{false, true} {
		b := newBank(t, func(w *stream.Writer) {
			w.Node(stream.Node{ID: "/n/a.md", Kind: "document", Text: "The journal is appended to."})
			w.Node(stream.Node{ID: "/n/b.md", Kind: "document", Text: "Журнал читается с начала."})
			w.Sweep()
		}, oneVoice...)
		names := map[string]string{"/n/a.md": "journal", "/n/b.md": "журнал"}
		asked := 0
		client := &scripted{
			extract: func(user string) string {
				return `{"mentions":[{"id":"","name":"` + names[passage(user)] + `","confidence":0.9}]}`
			},
			merge: func(user string) string {
				asked++
				if !same {
					return `{"same":[]}`
				}
				return `{"same":[{"pair":1,"keep":"journal"}]}` // the id without its kind, as models answer
			},
		}
		near := func(s *index.Session) {
			var hashes []string
			var vecs [][]float32
			for i, name := range []string{"journal", "журнал"} {
				v := make([]float32, 64)
				v[0], v[i+1] = 0.938, 0.3466 // cosine 0.88: under the merge, over the crossing
				hashes, vecs = append(hashes, index.ChunkHash(name)), append(vecs, v)
			}
			if err := s.Vectors().Append(hashes, vecs); err != nil {
				t.Fatal(err)
			}
		}
		runLLM(t, b, client, near)
		_, state := runLLM(t, b, client, nil)
		if asked != 1 {
			t.Errorf("same=%v: the pair was asked about %d times", same, asked)
		}
		if kept := state.Nodes["entity:journal"]; kept == nil || (state.Nodes["entity:журнал"] == nil) != same || (kept.Attrs["aliases"] == "журнал") != same {
			t.Errorf("same=%v: %+v", same, kept)
		}
	}
}
