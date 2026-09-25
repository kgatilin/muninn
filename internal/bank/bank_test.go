package bank

import (
	"reflect"
	"strings"
	"testing"
)

func TestSetAcceptsKnownKeysAndRefusesTheRest(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, err := New("work")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"embedder", "ollama:embeddinggemma:300m"},
		{"chunkers.text.budget", "1200"},
		{"search.seeds", "20"},
		{"search.edge_weights.links", "0.8"},
	} {
		if _, err := b.Set(kv[0], kv[1]); err != nil {
			t.Errorf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	if b.Embedder.Model != "embeddinggemma:300m" || b.Chunkers["text"].Budget != 1200 || b.Search.EdgeWeights["links"] != 0.8 {
		t.Errorf("bank = %+v", b)
	}
	for _, kv := range [][2]string{
		{"embedder", "cohere:x"},
		{"chunker", "cobol"},
		{"chunkers.cobol.budget", "500"},
		{"chunkers.text.budget", "12"},
		{"search.seeds", "0"},
		{"retention", "30d"},
	} {
		if _, err := b.Set(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%s was accepted", kv[0], kv[1])
		}
	}
	if _, err := b.Set("retention", "30d"); !strings.Contains(err.Error(), "search.seeds") {
		t.Errorf("an unknown key does not list the accepted ones: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, _ := New("notes")
	b.Connectors = []Connector{{Name: "fs", Command: []string{"muninn", "connect", "fs", "--root", "/tmp"}}}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Connectors) != 1 || got.Connectors[0].Command[4] != "/tmp" || got.Chunker != "text" {
		t.Errorf("loaded = %+v", got)
	}
	if names, _ := List(); len(names) != 1 || names[0] != "notes" {
		t.Errorf("list = %v", names)
	}
	if _, err := New("notes"); err == nil {
		t.Error("a second bank of the same name was accepted")
	}
	if _, err := New("Bad Name"); err == nil {
		t.Error("a bad name was accepted")
	}
}

func TestSemanticSettings(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, err := New("work")
	if err != nil {
		t.Fatal(err)
	}
	// A bank that says nothing has the layer off and the journal's bands.
	r := b.Semantic.Resolved()
	if r.Method != SemanticOff || r.Mentions != 5 || r.Rounds != 3 || r.Hops != 2 ||
		r.Checks["scale_free"].Params["low"] != 2.0 || r.Checks["scale_free"].Params["high"] != 3.5 ||
		r.Checks["fractal"].Params["low"] != 2.5 || r.Checks["fractal"].Params["high"] != 4.5 ||
		!*r.Checks["entity_budget"].Enabled || *r.Checks["singleton_share"].Enabled {
		t.Errorf("defaults = %+v", r)
	}
	for _, kv := range [][2]string{
		{"semantic.model", "ollama:qwen3:8b"},
		{"semantic.method", "llm"},
		{"semantic.mentions", "7"},
		{"semantic.hops", "3"},
		{"semantic.checks.fractal.enabled", "false"},
		{"semantic.checks.scale_free.high", "3.2"},
		{"semantic.checks.entity_budget.ratio", "0.5"},
		{"semantic.kinds", "turn, document,turn"},
	} {
		if _, err := b.Set(kv[0], kv[1]); err != nil {
			t.Errorf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	// The llm method needs a model, and takes one.
	fresh := &Bank{Name: "fresh"}
	if _, err := fresh.Set("semantic.method", "llm"); err == nil {
		t.Error("semantic.method=llm was accepted with no model")
	}
	for _, kv := range [][2]string{{"semantic.model", "anthropic"}, {"semantic.method", "llm"}, {"semantic.concurrency", "4"}, {"semantic.endpoint", "http://localhost:1"}} {
		if _, err := fresh.Set(kv[0], kv[1]); err != nil {
			t.Errorf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	if r := fresh.Semantic.Resolved(); r.Method != SemanticLLM || r.Concurrency != 4 {
		t.Errorf("llm settings = %+v", r)
	}
	for _, kv := range [][2]string{
		{"semantic.method", "magic"},
		{"semantic.concurrency", "0"},
		{"semantic.model", "qwen3"},
		{"semantic.rounds", "0"},
		{"semantic.checks.hairball.enabled", "true"},
		{"semantic.checks.fractal.ceiling", "1"},
		{"semantic.checks.fractal.enabled", "perhaps"},
		{"semantic.checks.scale_free.low", "9"},
		{"semantic.depth", "2"},
		{"semantic.kinds", "turn,entity"},
		{"semantic.kinds", "a kind"},
	} {
		if _, err := b.Set(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%s was accepted", kv[0], kv[1])
		}
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load("work")
	if err != nil {
		t.Fatal(err)
	}
	r = got.Semantic.Resolved()
	if r.Method != SemanticLLM || r.Mentions != 7 || r.Hops != 3 || *r.Checks["fractal"].Enabled ||
		r.Checks["scale_free"].Params["high"] != 3.2 || r.Checks["scale_free"].Params["low"] != 2.0 || r.Checks["entity_budget"].Params["ratio"] != 0.5 ||
		!reflect.DeepEqual(r.Kinds, []string{"document", "turn"}) || !r.Reads("turn") || r.Reads("session") {
		t.Errorf("after a round trip = %+v", r)
	}
	// An empty list reads every kind again.
	if _, err := got.Set("semantic.kinds", ""); err != nil || !got.Semantic.Reads("session") {
		t.Errorf("semantic.kinds= : %v, %+v", err, got.Semantic.Kinds)
	}
	// A connector cannot take the semantic layer's name.
	got.Connectors = []Connector{{Name: "~semantic", Command: []string{"true"}}}
	if err := got.Validate(); err == nil {
		t.Error("a connector named ~semantic was accepted")
	}
}

func TestSetSearchCuts(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	b, err := New("cuts")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"search.degree_cap.in", "200"}, {"search.degree_cap.of", "5"}, {"search.degree_cap.of", "0"},
		{"search.mute.add", "/repo/a.b/main.go"}, {"search.mute.add", "repo:x"}, {"search.mute.add", "repo:x"},
		{"search.mute.remove", "repo:x"},
	} {
		if _, err := b.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	for _, kv := range [][2]string{{"search.degree_cap.in", "-1"}, {"search.degree_cap.in", "many"}, {"search.mute.add", ""}, {"search.mute.remove", "never-muted"}} {
		if _, err := b.Set(kv[0], kv[1]); err == nil {
			t.Errorf("%s=%q was accepted", kv[0], kv[1])
		}
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load("cuts")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Search.DegreeCap) != 1 || got.Search.DegreeCap["in"] != 200 || len(got.Search.Mute) != 1 || got.Search.Mute[0] != "/repo/a.b/main.go" {
		t.Errorf("search = %+v", got.Search)
	}
	got.Search.DegreeCap["in"] = 0
	if err := got.Validate(); err == nil {
		t.Error("a hand-written zero cap validates")
	}
}
