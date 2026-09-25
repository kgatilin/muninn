package keys_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/keys"
	"github.com/kgatilin/muninn/stream"
)

// A design folder and a branch that carry one ticket are tied through it; a
// ticket one node holds is not made; a pattern taken away takes its nodes.
func TestKeysTieNodes(t *testing.T) {
	t.Setenv("MUNINN_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "stream.jsonl")
	var buf bytes.Buffer
	w := stream.NewWriter(&buf)
	for _, n := range []stream.Node{
		{ID: "/app/docs/design/APP-12-cache", Kind: "directory"},
		{ID: "/app/docs/design/APP-12-cache/design.md", Kind: "document", Text: "The cache sits behind the store."},
		{ID: "branch:/app@APP-12-cache-layer", Kind: "branch"},
		{ID: "branch:/app@APP-40-alone", Kind: "branch"},
	} {
		w.Node(n)
	}
	w.Flush()
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bank.New("test")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"embedder": "noop", "keys.ticket": `[A-Z]+-\d+`} {
		if _, err := b.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	b.Connectors = []bank.Connector{{Name: "src", Command: []string{"cat", file}}}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	run := func() *index.State {
		t.Helper()
		if err := index.Run(context.Background(), b, index.Options{After: keys.Stage(b)}); err != nil {
			t.Fatal(err)
		}
		state, err := index.LoadState(b.IndexDir())
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	state := run()
	if n := state.Nodes["ticket:APP-12"]; n == nil || n.Kind != "ticket" {
		t.Fatalf("ticket:APP-12 = %+v", n)
	}
	for _, from := range []string{"/app/docs/design/APP-12-cache", "/app/docs/design/APP-12-cache/design.md", "branch:/app@APP-12-cache-layer"} {
		if state.Edges[index.Edge{From: from, To: "ticket:APP-12", Kind: keys.EdgeHas}.Key()] == nil {
			t.Errorf("%s is not tied to its ticket", from)
		}
	}
	if state.Nodes["ticket:APP-40"] != nil {
		t.Error("a ticket one node holds was made")
	}

	if _, err := b.Set("keys.ticket", "off"); err != nil {
		t.Fatal(err)
	}
	if state = run(); state.Nodes["ticket:APP-12"] != nil {
		t.Error("the pattern went and its node stayed")
	}
}
