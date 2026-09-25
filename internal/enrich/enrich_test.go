package enrich_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/enrich"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/usage"
	"github.com/kgatilin/muninn/stream"
)

// scripted is a chat client whose answers a test writes, one function per
// pass, told apart by the schema asked for.
type scripted struct {
	mu              sync.Mutex
	extract, settle func(system, user string) string
	// consolidate answers the pass over the kept items; nil merges nothing.
	consolidate func(user string) string
	extracted   []string
	// pressed counts the passes asked to make the items fewer and broader.
	pressed int
}

func (*scripted) ID() string { return "fake:model" }

func (s *scripted) Complete(_ context.Context, system, user string, schema json.RawMessage) (string, usage.Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !json.Valid(schema) {
		return "", usage.Tokens{}, fmt.Errorf("schema is not JSON: %s", schema)
	}
	if strings.Contains(string(schema), `"merges"`) {
		if strings.Contains(system, "fewer and broader") {
			s.pressed++
		}
		if s.consolidate == nil {
			return `{"merges":[]}`, usage.Tokens{}, nil
		}
		return s.consolidate(user), usage.Tokens{In: 5, Out: 1, Requests: 1}, nil
	}
	if strings.Contains(string(schema), `"decisions"`) {
		return s.settle(system, user), usage.Tokens{In: 20, Out: 4, Requests: 1}, nil
	}
	s.extracted = append(s.extracted, user)
	return s.extract(system, user), usage.Tokens{In: 10, Out: 2, Requests: 1}, nil
}

type node struct {
	id, kind, text string
	minute         int
	shown          []string
}

// newBank is a bank whose one connector cats the nodes, a minute apart, the
// turns of one session chained by `next`.
func newBank(t *testing.T, settings []string, nodes ...node) (*bank.Bank, func(...node)) {
	t.Helper()
	t.Setenv("MUNINN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	write := func(nodes ...node) {
		var buf bytes.Buffer
		w := stream.NewWriter(&buf)
		previous := ""
		for _, n := range nodes {
			at := time.Date(2026, 1, 2, 10, n.minute, 0, 0, time.UTC)
			w.Node(stream.Node{ID: n.id, Kind: n.kind, Text: n.text, At: &at, Attrs: map[string]string{"session": "s1", "repo": "app"}})
			if n.kind == "message" {
				if previous != "" {
					w.Edge(stream.Edge{From: previous, To: n.id, Kind: "next"})
				}
				previous = n.id
			}
			for _, r := range n.shown {
				w.Edge(stream.Edge{From: n.id, To: r, Kind: "injected"})
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(nodes...)
	b, err := bank.New("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range append([]string{"embedder=noop", "enrich.model=ollama"}, settings...) {
		k, v, _ := strings.Cut(kv, "=")
		if _, err := b.Set(k, v); err != nil {
			t.Fatalf("%s: %v", kv, err)
		}
	}
	b.Connectors = []bank.Connector{{Name: "src", Command: []string{"cat", file}}}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	return b, write
}

func run(t *testing.T, b *bank.Bank, client *scripted) (map[string]enrich.Stats, *index.State) {
	t.Helper()
	var st map[string]enrich.Stats
	err := index.Run(context.Background(), b, index.Options{After: func(ctx context.Context, s *index.Session) (err error) {
		st, err = enrich.Run(ctx, s, client)
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

func kept(state *index.State, name string) map[string]*index.Node {
	out := map[string]*index.Node{}
	for id, n := range state.Nodes {
		if index.Enrichment(n) == name {
			out[id] = n
		}
	}
	return out
}

func states(state *index.State, from, to string) bool {
	return state.Edges[(index.Edge{From: from, To: to, Kind: enrich.EdgeStates}).Key()] != nil
}

var passageNo = regexp.MustCompile(`--- passage (\d+) · \S+ · (\S+)`)

// numberOf is the number a passage has in a prompt.
func numberOf(t *testing.T, user, id string) string {
	t.Helper()
	for _, m := range passageNo.FindAllStringSubmatch(user, -1) {
		if m[2] == id {
			return m[1]
		}
	}
	t.Fatalf("%s is not among the passages:\n%s", id, user)
	return ""
}

func TestALaterPassageRewritesTheItem(t *testing.T) {
	turns := []node{
		{id: "turn:1", kind: "message", minute: 1, text: "User: add the repository\n\nAssistant: Indexing it with the local embedder."},
		{id: "turn:2", kind: "message", minute: 2, text: "User: not the local one, use the hosted embedder here\n\nAssistant: Switched, and I will keep to the hosted one."},
		{id: "/doc.md", kind: "document", minute: 3, text: "A document the enrichment does not select."},
	}
	b, write := newBank(t, []string{"enrich.process.preset=process", "enrich.process.neighbours=1"}, turns...)
	client := &scripted{
		extract: func(system, user string) string {
			if !strings.Contains(system, "standing rules of how work is done") || !strings.Contains(system, `"from"`) {
				t.Errorf("the prompt is not the preset's within the frame:\n%s", system)
			}
			if strings.Contains(user, "/doc.md") {
				t.Errorf("a node the enrichment does not select was read:\n%s", user)
			}
			switch {
			case strings.Contains(user, "the small hosted model is enough"):
				return `{"items":[{"text":"Use the small hosted embedding model.","from":[` + numberOf(t, user, "turn:3") + `]}]}`
			case strings.Contains(user, "forget the embedder rule"):
				return `{"items":[{"text":"Drop the rule about the embedder.","from":[` + numberOf(t, user, "turn:4") + `]}]}`
			}
			// Both turns are read in one call, the whole of each.
			if !strings.Contains(user, "Indexing it with the local embedder.") || !strings.Contains(user, "I will keep to the hosted one.") {
				t.Errorf("the neighbourhood is not both turns whole:\n%s", user)
			}
			return `{"items":[{"text":"Use the hosted embedder for banks of this project.","from":[` + numberOf(t, user, "turn:2") + `,99]},{"text":"","from":[1]},{"text":"Supported by nothing.","from":[]}]}`
		},
		settle: func(_, user string) string { return `{"decisions":[{"item":1,"action":"new","id":"","text":""}]}` },
	}
	st, state := run(t, b, client)
	if got := st["process"]; got.Selected != 2 || got.Pending != 2 || got.Groups != 1 || got.Added != 1 {
		t.Fatalf("stats = %+v", got)
	}
	items := kept(state, "process")
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	var id string
	for id = range items {
	}
	n := items[id]
	if !strings.HasPrefix(id, "rule:") || n.Kind != "rule" || n.Chunks[0].Text != "Use the hosted embedder for banks of this project." ||
		n.Attrs["type"] != "process" || n.Attrs["repo"] != "app" || !n.At.Equal(time.Date(2026, 1, 2, 10, 2, 0, 0, time.UTC)) {
		t.Errorf("item %s = %+v", id, n)
	}
	if !states(state, "turn:2", id) || states(state, "turn:1", id) {
		t.Error("the edges are not those of the passages the item came from")
	}

	// Nothing new: nothing is read.
	calls := len(client.extracted)
	if st, _ := run(t, b, client); st["process"].Pending != 0 || len(client.extracted) != calls {
		t.Errorf("a second run read again: %+v", st["process"])
	}

	// The session goes on. The agent is shown the rule and the user corrects
	// it: the rule is marked among the kept ones, and its text becomes what
	// the model says holds now, under the same id, with the new turn's edge.
	client.settle = func(_, user string) string {
		if !strings.Contains(user, "- "+id+" (shown): Use the hosted embedder") {
			t.Errorf("the shown rule is not marked:\n%s", user)
		}
		return `{"decisions":[{"item":1,"action":"update","id":"` + strings.TrimPrefix(id, "rule:") + ` (shown)","text":"Use the small hosted embedding model for banks of this project."}]}`
	}
	turns = append(turns, node{id: "turn:3", kind: "message", minute: 4, text: "User: the small hosted model is enough\n\nAssistant: Done.", shown: []string{id}})
	write(turns...)
	st, state = run(t, b, client)
	if got := st["process"]; got.Pending != 1 || got.Updated != 1 {
		t.Fatalf("stats = %+v", got)
	}
	if got := kept(state, "process"); len(got) != 1 || got[id].Chunks[0].Text != "Use the small hosted embedding model for banks of this project." {
		t.Errorf("items = %v", got)
	}
	if !states(state, "turn:2", id) || !states(state, "turn:3", id) {
		t.Error("the item lost an edge or did not get the new one")
	}

	// A rule the user cancels goes.
	client.settle = func(_, user string) string {
		return `{"decisions":[{"item":1,"action":"retire","id":"` + id + `","text":""}]}`
	}
	turns = append(turns, node{id: "turn:4", kind: "message", minute: 5, text: "User: forget the embedder rule"})
	write(turns...)
	st, state = run(t, b, client)
	if st["process"].Retired != 1 || len(kept(state, "process")) != 0 {
		t.Errorf("stats = %+v, items = %v", st["process"], kept(state, "process"))
	}
}

// An enrichment of the bank's own: documents under a path, a prompt, a field
// an item carries, two supporting passages asked for.
func TestACustomEnrichment(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("Find the decisions these documents record.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := newBank(t, []string{
		"enrich.decisions.paths=**/design/**/*.md", "enrich.decisions.prompt=@" + prompt, "enrich.decisions.kind=decision",
		"enrich.decisions.fields=scope", "enrich.decisions.votes=2", "enrich.decisions.neighbours=2",
	},
		node{id: "/app/design/store/plan.md", kind: "document", minute: 1, text: "Storage goes behind one interface."},
		node{id: "/app/design/cache/plan.md", kind: "document", minute: 2, text: "The cache is reached through the storage interface."},
		node{id: "/app/notes/todo.md", kind: "document", minute: 3, text: "Buy milk."},
	)
	client := &scripted{
		extract: func(system, user string) string {
			if !strings.HasPrefix(system, "Find the decisions these documents record.") || !strings.Contains(system, `"scope": "…"`) {
				t.Errorf("system = %s", system)
			}
			if strings.Contains(user, "todo.md") {
				t.Errorf("a path outside the glob was read:\n%s", user)
			}
			return `{"items":[{"text":"Reach storage through its interface only.","from":[1,2],"scope":"storage"},{"text":"One document says this.","from":[1],"scope":"x"}]}`
		},
		settle: func(_, user string) string {
			if strings.Contains(user, "One document says this.") {
				t.Errorf("an item with one vote was settled:\n%s", user)
			}
			return `{"decisions":[{"item":1,"action":"same","id":"decision:000000000000","text":""}]}`
		},
	}
	st, state := run(t, b, client)
	items := kept(state, "decisions")
	if st["decisions"].Added != 1 || len(items) != 1 {
		t.Fatalf("stats = %+v, items = %v: an id that is not kept is a new item", st["decisions"], items)
	}
	for id, n := range items {
		if !strings.HasPrefix(id, "decision:") || n.Kind != "decision" || n.Attrs["scope"] != "storage" || n.Attrs["type"] != "decisions" {
			t.Errorf("item %s = %+v", id, n)
		}
		if !states(state, "/app/design/store/plan.md", id) || !states(state, "/app/design/cache/plan.md", id) {
			t.Error("the item is not tied to both documents")
		}
	}
	if n := enrich.Reset(state, "decisions"); n != 1 || len(state.Enriched) != 0 {
		t.Errorf("reset took %d items and left %d nodes read", n, len(state.Enriched))
	}
}

// Kept items about one matter become one: the item that stays takes the text
// the model gives it and the passages behind the ones that go.
func TestMerge(t *testing.T) {
	b, write := newBank(t, []string{"enrich.process.preset=process"},
		node{id: "m:1", kind: "message", minute: 1, text: "User: storage goes behind one interface"},
		node{id: "m:2", kind: "message", minute: 2, text: "User: the cache never opens the database itself"},
	)
	client := &scripted{
		// A passage read again beside a new one states nothing new: only the
		// passage that called the neighbourhood speaks here.
		extract: func(_, user string) string {
			rules := map[string]string{"m:1": "Reach storage through its interface.", "m:2": "The cache must not open the database.", "m:3": "Nothing but the storage package touches the database."}
			var items []string
			for _, id := range []string{"m:3", "m:2", "m:1"} {
				if strings.Contains(user, " "+id+"\n") {
					items = append(items, `{"text":"`+rules[id]+`","from":[`+numberOf(t, user, id)+`]}`)
					if id == "m:3" {
						break
					}
				}
			}
			return `{"items":[` + strings.Join(items, ",") + `]}`
		},
		settle: func(_, _ string) string {
			return `{"decisions":[{"item":1,"action":"new","id":"","with":[],"text":""},{"item":2,"action":"new","id":"","with":[],"text":""}]}`
		},
	}
	_, state := run(t, b, client)
	var ids []string
	for id := range kept(state, "process") {
		ids = append(ids, id)
	}
	if len(ids) != 2 {
		t.Fatalf("items = %v", ids)
	}
	client.settle = func(_, _ string) string {
		return `{"decisions":[{"item":1,"action":"merge","id":"` + ids[0] + `","with":["` + ids[1] + `","rule:000000000000"],"text":"Only the storage package touches the database; everything else goes through its interface."}]}`
	}
	write(
		node{id: "m:1", kind: "message", minute: 1, text: "User: storage goes behind one interface"},
		node{id: "m:2", kind: "message", minute: 2, text: "User: the cache never opens the database itself"},
		node{id: "m:3", kind: "message", minute: 3, text: "User: nothing but the storage package touches the database"},
	)
	st, state := run(t, b, client)
	items := kept(state, "process")
	if st["process"].Merged != 1 || len(items) != 1 || !strings.HasPrefix(items[ids[0]].Chunks[0].Text, "Only the storage package") {
		t.Fatalf("stats = %+v, items = %v", st["process"], items)
	}
	for _, from := range []string{"m:1", "m:2", "m:3"} {
		if !states(state, from, ids[0]) {
			t.Errorf("%s does not support the merged item", from)
		}
	}
	for _, e := range state.Edges {
		if e.To == ids[1] {
			t.Errorf("an edge to the item that went: %+v", e)
		}
	}
}

// The kept items are looked over as the passages were: each with the items
// nearest to it, and those the model says are one become one. An item that has
// been looked at as it stands is not asked about again.
func TestConsolidate(t *testing.T) {
	b, _ := newBank(t, []string{"enrich.process.preset=process"},
		node{id: "m:1", kind: "message", minute: 1, text: "User: storage goes behind one interface"},
		node{id: "m:2", kind: "message", minute: 2, text: "User: the cache never opens the database itself"},
	)
	asked := 0
	client := &scripted{
		extract: func(_, user string) string {
			return `{"items":[{"text":"Reach storage through its interface.","from":[` + numberOf(t, user, "m:1") + `]},{"text":"The cache must not open the database.","from":[` + numberOf(t, user, "m:2") + `]}]}`
		},
		settle: func(_, _ string) string {
			return `{"decisions":[{"item":1,"action":"new","id":"","with":[],"text":""},{"item":2,"action":"new","id":"","with":[],"text":""}]}`
		},
	}
	client.consolidate = func(user string) string {
		asked++
		ids := regexp.MustCompile(`rule:[0-9a-f]{12}`).FindAllString(user, -1)
		if len(ids) != 2 {
			t.Fatalf("the items shown:\n%s", user)
		}
		return `{"merges":[{"id":"` + ids[0] + `","with":["` + ids[1] + `"],"text":"Only the storage package touches the database."}]}`
	}
	st, state := run(t, b, client)
	items := kept(state, "process")
	if st["process"].Merged != 1 || len(items) != 1 {
		t.Fatalf("stats = %+v, items = %v", st["process"], items)
	}
	for id, n := range items {
		if n.Chunks[0].Text != "Only the storage package touches the database." || n.Attrs["type"] != "process" || !states(state, "m:1", id) || !states(state, "m:2", id) {
			t.Errorf("the merged item %s = %+v", id, n)
		}
	}
	// The merged item is new as it stands and is looked at once more, alone
	// among the kept ones: there is nothing to show it with, and nobody is asked.
	if asked != 1 {
		t.Errorf("the model was asked %d times", asked)
	}
}

// An enrichment over its ratio drops nothing: it is pressed, the model merging
// what it will, and what stays apart is kept however many that is.
func TestOverTheRatioNothingIsDropped(t *testing.T) {
	var nodes []node
	for i := 1; i <= 12; i++ {
		nodes = append(nodes, node{id: fmt.Sprintf("m:%d", i), kind: "message", minute: i, text: fmt.Sprintf("User: rule number %d holds", i)})
	}
	b, _ := newBank(t, []string{"enrich.process.preset=process"}, nodes...)
	client := &scripted{
		extract: func(_, user string) string {
			var items []string
			for _, m := range regexp.MustCompile(`· m:(\d+)\n`).FindAllStringSubmatch(user, -1) {
				items = append(items, fmt.Sprintf(`{"text":"Rule %s holds.","from":[%s]}`, m[1], numberOf(t, user, "m:"+m[1])))
			}
			return `{"items":[` + strings.Join(items, ",") + `]}`
		},
		settle: func(_, user string) string {
			var ds []string
			for i := range regexp.MustCompile(`(?m)^\d+\. `).FindAllString(user, -1) {
				ds = append(ds, fmt.Sprintf(`{"item":%d,"action":"new","id":"","with":[],"text":""}`, i+1))
			}
			return `{"decisions":[` + strings.Join(ds, ",") + `]}`
		},
	}
	merged := false
	client.consolidate = func(user string) string {
		ids := regexp.MustCompile(`rule:[0-9a-f]{12}`).FindAllString(user, -1)
		if client.pressed == 0 || merged || len(ids) < 2 {
			return `{"merges":[]}`
		}
		merged = true
		return `{"merges":[{"id":"` + ids[0] + `","with":["` + ids[1] + `"],"text":"Both rules hold."}]}`
	}
	st, state := run(t, b, client)
	if client.pressed == 0 {
		t.Fatal("twelve items against a floor of ten were not pressed")
	}
	if got := len(kept(state, "process")); st["process"].Merged != 1 || got != 11 {
		t.Fatalf("stats = %+v, %d items kept: want 1 merged and the other 11 kept", st["process"], got)
	}
}

// A batch is settled in one call, and an item that repeats another of the
// batch lands on the item the first one became.
func TestABatchIsSettledTogether(t *testing.T) {
	b, _ := newBank(t, []string{"enrich.process.preset=process", "enrich.process.neighbours=1"},
		node{id: "m:1", kind: "message", minute: 1, text: "User: always run the linter"},
		node{id: "m:2", kind: "message", minute: 2, text: "User: never skip the tests"},
		node{id: "m:3", kind: "message", minute: 3, text: "User: run the linter, I said"},
		node{id: "m:4", kind: "message", minute: 4, text: "User: and keep commits small"},
	)
	settles := 0
	client := &scripted{
		extract: func(_, user string) string {
			var items []string
			for _, m := range regexp.MustCompile(`· (m:\d+)\n`).FindAllStringSubmatch(user, -1) {
				items = append(items, fmt.Sprintf(`{"text":"Rule of %s.","from":[%s]}`, m[1], numberOf(t, user, m[1])))
			}
			return `{"items":[` + strings.Join(items, ",") + `]}`
		},
		settle: func(_, user string) string {
			settles++
			var ds []string
			for i, line := range regexp.MustCompile(`(?m)^\d+\. .*$`).FindAllString(user, -1) {
				if strings.Contains(line, "m:3") {
					first := numberOfItem(t, user, "m:1")
					ds = append(ds, fmt.Sprintf(`{"item":%d,"action":"same","id":"item %s","with":[],"text":""}`, i+1, first))
					continue
				}
				ds = append(ds, fmt.Sprintf(`{"item":%d,"action":"new","id":"","with":[],"text":""}`, i+1))
			}
			return `{"decisions":[` + strings.Join(ds, ",") + `]}`
		},
	}
	_, state := run(t, b, client)
	if settles != 1 {
		t.Errorf("the batch was settled in %d calls", settles)
	}
	items := kept(state, "process")
	if len(items) != 3 {
		t.Fatalf("%d items kept, want 3", len(items))
	}
	for id, n := range items {
		if n.Chunks[0].Text == "Rule of m:1." && !states(state, "m:3", id) {
			t.Error("the repeat is not behind the item it repeats")
		}
	}
}

func numberOfItem(t *testing.T, user, marker string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^(\d+)\. .*` + regexp.QuoteMeta(marker)).FindStringSubmatch(user)
	if m == nil {
		t.Fatalf("no item of %s in:\n%s", marker, user)
	}
	return m[1]
}

func TestSettings(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, err := bank.New("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{
		"enrich.arch.preset=nothing", "enrich.Arch.preset=process", "enrich.arch.fields=text",
		"enrich.arch.ratio=much", "enrich.arch.votes=9", "enrich.arch=on",
	} {
		k, v, _ := strings.Cut(kv, "=")
		if _, err := b.Set(k, v); err == nil {
			t.Errorf("%s was accepted", kv)
		}
	}
	// An enrichment is set a key at a time and is left out until it is whole.
	// The architecture preset has no paths of its own: where a project keeps
	// its documents is the bank's to say.
	if _, err := b.Set("enrich.arch.preset", "architecture"); err != nil {
		t.Fatal(err)
	}
	if got := b.Enrich.Sets["arch"].Missing(); !strings.Contains(got, "paths") {
		t.Errorf("missing = %q", got)
	}
	if _, err := b.Set("enrich.arch.paths", "**/design/**"); err != nil {
		t.Fatal(err)
	}
	if got := b.Enrich.Sets["arch"]; got.Missing() != "" || !got.Selects("document", "/app/design/store/plan.md") || got.Selects("document", "/app/notes/todo.md") {
		t.Errorf("the enrichment = %+v, missing %q", got, got.Missing())
	}
	if _, err := b.Set("enrich.arch", "off"); err != nil || len(b.Enrich.Sets) != 0 {
		t.Errorf("off: %v, %v", err, b.Enrich.Sets)
	}
}

// A later statement speaks against one part of an item another enrichment
// keeps: that item loses the part and nothing else, stays its enrichment's, and
// has the statement among the nodes behind it.
func TestAStatementSpeaksAgainstAPartOfAnotherReadingsItem(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("Find the rules of the design."), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := node{id: "/app/design/plan.md", kind: "document", minute: 1, text: "Storage goes behind one interface. Every plugin registers itself at start."}
	b, write := newBank(t, []string{"enrich.design.paths=**/design/*.md", "enrich.design.prompt=@" + prompt, "enrich.process.preset=process"}, doc)
	client := &scripted{
		extract: func(_, user string) string {
			if strings.Contains(user, "plan.md") {
				return `{"items":[{"text":"Storage goes behind one interface. Every plugin registers itself at start.","from":[` + numberOf(t, user, doc.id) + `]}]}`
			}
			return `{"items":[{"text":"Plugins are listed in the config, none registers itself.","from":[` + numberOf(t, user, "m:1") + `]}]}`
		},
		settle: func(_, user string) string {
			_, others, shown := strings.Cut(user, "Items of other readings:")
			if !shown {
				return `{"decisions":[{"item":1,"action":"new","id":"","with":[],"text":"","against":[]}]}`
			}
			id := regexp.MustCompile(`rule:[0-9a-f]{12}`).FindString(others)
			return `{"decisions":[{"item":1,"action":"new","id":"","with":[],"text":"","against":[{"id":"` + id + `","text":"Storage goes behind one interface."}]}]}`
		},
	}
	run(t, b, client)
	write(doc, node{id: "m:1", kind: "message", minute: 5, text: "User: plugins are listed in the config, none registers itself"})
	st, state := run(t, b, client)
	design := kept(state, "design")
	if len(design) != 1 || len(kept(state, "process")) != 1 || st["process"].Updated != 1 {
		t.Fatalf("design %d, process %d, %+v", len(design), len(kept(state, "process")), st)
	}
	for id, n := range design {
		if got := n.Chunks[0].Text; got != "Storage goes behind one interface." {
			t.Errorf("the item reads %q", got)
		}
		if !states(state, "m:1", id) || !states(state, doc.id, id) || n.At.Minute() != 5 {
			t.Errorf("behind the item: m:1 %v, the document %v, at %v", states(state, "m:1", id), states(state, doc.id, id), n.At)
		}
	}
}
