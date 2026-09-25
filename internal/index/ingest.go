package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/chunk"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/usage"
	"github.com/kgatilin/muninn/stream"
)

type Options struct {
	// Connector limits the run to one connector; empty means all of them.
	Connector string
	// Full forgets the cursors, so a resuming connector starts over.
	Full bool
	Log  io.Writer
	// After runs once the connectors have committed, under the same lock: the
	// semantic stage. It is a function so that this package imports nothing of
	// what builds on it.
	After func(context.Context, *Session) error
}

// embedSlice is how many chunks are embedded between two appends to the
// vector file: what an interrupted run keeps.
const embedSlice = 256

type indexer struct {
	bank    *bank.Bank
	dir     string
	state   *State
	vectors *Vectors
	emb     embed.Embedder
	log     io.Writer
}

// Run runs the bank's connectors once each, committing after every cursor and
// at the end of every stream.
func Run(ctx context.Context, b *bank.Bank, o Options) error {
	if o.Log == nil {
		o.Log = io.Discard
	}
	connectors := b.Connectors
	if o.Connector != "" {
		c, ok := b.Connector(o.Connector)
		if !ok {
			return fmt.Errorf("bank %q has no connector %q", b.Name, o.Connector)
		}
		connectors = []bank.Connector{c}
	}
	if len(connectors) == 0 {
		return fmt.Errorf("bank %q has no connectors; `muninn connector add %s <name> -- <command>`", b.Name, b.Name)
	}

	ix, release, err := open(b)
	if err != nil {
		return err
	}
	defer release()
	ix.log = o.Log

	for _, c := range connectors {
		if o.Full {
			delete(ix.state.Cursors, c.Name)
		}
		if err := ix.runConnector(ctx, c); err != nil {
			return fmt.Errorf("connector %s: %w", c.Name, err)
		}
	}
	if o.After != nil {
		return o.After(ctx, &Session{ix: ix})
	}
	return nil
}

// RemoveConnectorNodes deletes what a connector produced, with its cursor.
func RemoveConnectorNodes(b *bank.Bank, name string) (int, error) {
	ix, release, err := open(b)
	if err != nil {
		return 0, err
	}
	defer release()
	removed := 0
	for id, n := range ix.state.Nodes {
		if n.Connector == name {
			ix.deleteNode(id)
			removed++
		}
	}
	for k, e := range ix.state.Edges {
		if e.Connector == name {
			delete(ix.state.Edges, k)
		}
	}
	for id, owner := range ix.state.Compacted {
		if owner == name {
			delete(ix.state.Compacted, id)
		}
	}
	delete(ix.state.Cursors, name)
	delete(ix.state.Runs, name)
	return removed, ix.save()
}

func open(b *bank.Bank) (*indexer, func(), error) {
	dir := b.IndexDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	release, err := lock(dir)
	if err != nil {
		return nil, nil, err
	}
	ix := &indexer{bank: b, dir: dir, log: io.Discard}
	if ix.emb, err = embed.New(b.Embedder); err == nil {
		if ix.state, err = LoadState(dir); err == nil {
			ix.vectors, err = OpenVectors(dir, ix.emb.Key())
		}
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return ix, release, nil
}

func (ix *indexer) runConnector(ctx context.Context, c bank.Connector) error {
	name := c.Command[0]
	if name == "muninn" {
		// The shipped connectors are this binary, wherever it was started from.
		if self, err := os.Executable(); err == nil {
			name = self
		}
	}
	cmd := exec.CommandContext(ctx, name, c.Command[1:]...)
	cmd.Env = append(os.Environ(), "MUNINN_BANK="+ix.bank.Name, "MUNINN_CURSOR="+ix.state.Cursors[c.Name])
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	run := RunInfo{At: time.Now()}
	seenNodes, seenEdges := map[string]bool{}, map[string]bool{}
	swept := false
	apply := func() error {
		r := stream.NewReader(out)
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			switch {
			case rec.Node != nil && ix.state.Compacted[rec.Node.ID] != "":
			case rec.Edge != nil && (ix.state.Compacted[rec.Edge.From] != "" || ix.state.Compacted[rec.Edge.To] != ""):
			case rec.Node != nil:
				if old := ix.state.Nodes[rec.Node.ID]; old != nil && old.Connector == SemanticConnector {
					return fmt.Errorf("node %s is the semantic layer's; a connector cannot emit it", rec.Node.ID)
				}
				changed, err := ix.applyNode(c.Name, rec.Node)
				if err != nil {
					return err
				}
				seenNodes[rec.Node.ID] = true
				run.Nodes++
				if changed {
					run.Changed++
				}
			case rec.Edge != nil:
				e := &Edge{From: rec.Edge.From, To: rec.Edge.To, Kind: rec.Edge.Kind, Weight: rec.Edge.Weight, Connector: c.Name}
				if old := ix.state.Edges[e.key()]; old != nil && old.Connector == SemanticConnector {
					return fmt.Errorf("edge %s -%s-> %s is the semantic layer's; a connector cannot emit it", e.From, e.Kind, e.To)
				}
				ix.state.Edges[e.key()] = e
				seenEdges[e.key()] = true
			case rec.Delete != nil:
				if n, ok := ix.state.Nodes[rec.Delete.ID]; ok && n.Connector == c.Name {
					ix.deleteNode(rec.Delete.ID)
					run.Deleted++
				}
			case rec.Cursor != nil:
				ix.state.Cursors[c.Name] = rec.Cursor.Value
				embedded, err := ix.commit(ctx)
				run.Embedded += embedded
				if err != nil {
					return err
				}
			case rec.Sweep != nil:
				swept = true
			}
		}
	}

	if err := apply(); err != nil {
		// A record the engine refuses fails the run with nothing of this
		// stream committed past its last cursor.
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	waitErr := cmd.Wait()
	if waitErr == nil && swept {
		for id, n := range ix.state.Nodes {
			if n.Connector == c.Name && !seenNodes[id] {
				ix.deleteNode(id)
				run.Deleted++
			}
		}
		for k, e := range ix.state.Edges {
			if e.Connector == c.Name && !seenEdges[k] {
				delete(ix.state.Edges, k)
			}
		}
	}
	if waitErr != nil {
		run.Err = waitErr.Error()
	}
	embedded, err := ix.commit(ctx)
	run.Embedded += embedded
	if err != nil {
		return err
	}
	ix.state.Runs[c.Name] = run
	if err := ix.state.save(ix.dir); err != nil {
		return err
	}
	fmt.Fprintf(ix.log, "%s: %d nodes, %d changed, %d deleted, %d chunks embedded\n", c.Name, run.Nodes, run.Changed, run.Deleted, run.Embedded)
	if waitErr != nil {
		return fmt.Errorf("exited with %w; what it emitted is committed, its sweep is not", waitErr)
	}
	return nil
}

func (ix *indexer) applyNode(connector string, n *stream.Node) (changed bool, err error) {
	name := n.Chunker
	if name == "" {
		name = ix.bank.Chunker
	}
	ck, ok := chunk.Get(name)
	if !ok {
		return false, fmt.Errorf("node %s names chunker %q, which this engine does not have; accepted: %s", n.ID, name, strings.Join(chunk.Names(), ", "))
	}
	params := ix.bank.Chunkers[name]
	hash := n.Hash
	if hash == "" && n.Text != "" {
		hash = sum(n.Text)
	}
	node := &Node{ID: n.ID, Kind: n.Kind, Hash: hash, Connector: connector, Attrs: n.Attrs, Recipe: chunk.Recipe(ck, params)}
	if n.At != nil {
		node.At = *n.At
	}
	if old := ix.state.Nodes[n.ID]; old != nil && old.Hash == node.Hash && old.Recipe == node.Recipe {
		node.Chunks = old.Chunks
	} else {
		changed = true
		line, pos := 1, 0
		for _, c := range ck.Chunk(n.Text, params) {
			line += strings.Count(n.Text[pos:c.Start], "\n")
			body := n.Text[c.Start:c.End]
			out := Chunk{Prefix: c.Prefix, Text: body, Line: line, EndLine: line + strings.Count(body, "\n")}
			out.Hash = sum(out.EmbedText())
			node.Chunks = append(node.Chunks, out)
			pos = c.Start
		}
	}
	ix.state.Nodes[n.ID] = node
	return changed, nil
}

// deleteNode removes the node with the edges its own connector stated and the
// semantic ones built on its text. An edge another connector stated stays and
// dangles: that connector still asserts it, may never say so again if it
// resumes from a cursor, and the edge takes effect once more when the node
// comes back.
func (ix *indexer) deleteNode(id string) {
	owner := ""
	if n := ix.state.Nodes[id]; n != nil {
		owner = n.Connector
	}
	delete(ix.state.Nodes, id)
	for k, e := range ix.state.Edges {
		if (e.From == id || e.To == id) && (e.Connector == owner || e.Connector == SemanticConnector) {
			delete(ix.state.Edges, k)
		}
	}
}

// commit embeds what has no vector yet, then writes the snapshot. Vectors land
// first: they are keyed by content, so a vector without a node is harmless and
// a node without a vector is not.
func (ix *indexer) commit(ctx context.Context) (embedded int, err error) {
	missing := map[string]string{}
	for _, n := range ix.state.Nodes {
		for _, c := range n.Chunks {
			if !ix.vectors.Has(c.Hash) {
				missing[c.Hash] = c.EmbedText()
			}
		}
	}
	hashes := make([]string, 0, len(missing))
	for h := range missing {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for start := 0; start < len(hashes); start += embedSlice {
		part := hashes[start:min(start+embedSlice, len(hashes))]
		texts := make([]string, len(part))
		for i, h := range part {
			texts[i] = missing[h]
		}
		vecs, tokens, err := ix.emb.Embed(ctx, texts)
		ix.bank.RecordUsage(usage.Embed, ix.bank.Embedder.String(), tokens)
		if err != nil {
			return embedded, fmt.Errorf("embedding: %w", err)
		}
		if err := ix.vectors.Append(part, vecs); err != nil {
			return embedded, err
		}
		embedded += len(part)
		fmt.Fprintf(ix.log, "embedded %d/%d\n", embedded, len(hashes))
	}
	return embedded, ix.save()
}

func (ix *indexer) save() error {
	if err := ix.state.save(ix.dir); err != nil {
		return err
	}
	return saveGob(ix.dir+"/lexical.gob", buildLexical(ix.state))
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}
