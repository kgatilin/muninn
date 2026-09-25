package index

import (
	"context"
	"io"
	"strings"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/chunk"
	"github.com/kgatilin/muninn/internal/embed"
)

// SemanticConnector owns the nodes and edges the engine builds itself. A bank's
// connector cannot carry the name — `~` is outside what a name may hold — so no
// connector's delete, sweep or removal reaches what it owns.
const SemanticConnector = "~semantic"

// EnrichConnector owns the items an enrichment draws from the graph and the
// edges from the nodes that support them. Its nodes are text like any
// connector's, so the semantic stage may read them and an entity mention them.
func EnrichConnector(name string) string { return enrichPrefix + name }

const enrichPrefix = "~enrich:"

// Enrichment is the name of the enrichment that made the node, or "".
func Enrichment(n *Node) string {
	name, _ := strings.CutPrefix(n.Connector, enrichPrefix)
	if name == n.Connector {
		return ""
	}
	return name
}

// Key identifies an edge in State.Edges.
func (e Edge) Key() string { return e.key() }

// Session is a bank's index held open under the writer's lock, for a stage
// that writes to the graph without being a connector. Run hands one to
// Options.After; Open takes the lock for a command of its own.
type Session struct {
	ix      *indexer
	release func()
}

func Open(b *bank.Bank, log io.Writer) (*Session, error) {
	ix, release, err := open(b)
	if err != nil {
		return nil, err
	}
	if log != nil {
		ix.log = log
	}
	return &Session{ix: ix, release: release}, nil
}

// Close releases the bank. A Session handed out by Run is closed by Run.
func (s *Session) Close() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

func (s *Session) Bank() *bank.Bank         { return s.ix.bank }
func (s *Session) State() *State            { return s.ix.state }
func (s *Session) Vectors() *Vectors        { return s.ix.vectors }
func (s *Session) Embedder() embed.Embedder { return s.ix.emb }
func (s *Session) Log() io.Writer           { return s.ix.log }

// Lexical is the lexical index of the state as it stands, not as it was saved.
func (s *Session) Lexical() *Lexical { return buildLexical(s.ix.state) }

// Commit embeds what has no vector yet and writes the snapshot with its
// lexical index, as a connector's cursor does.
func (s *Session) Commit(ctx context.Context) (embedded int, err error) { return s.ix.commit(ctx) }

// SaveState writes the graph alone: what an interrupted stage keeps. The
// lexical index and the vectors catch up at the next Commit.
func (s *Session) SaveState() error { return s.ix.state.save(s.ix.dir) }

// NewTextNode is a node the engine makes for itself: its text is one chunk,
// as the `none` chunker would leave it.
func NewTextNode(id, kind, connector, text string, attrs map[string]string) *Node {
	ck, _ := chunk.Get("none")
	n := &Node{ID: id, Kind: kind, Hash: sum(text), Connector: connector, Attrs: attrs, Recipe: chunk.Recipe(ck, chunk.Params{})}
	if text != "" {
		c := Chunk{Text: text, Line: 1, EndLine: 1}
		c.Hash = sum(c.EmbedText())
		n.Chunks = []Chunk{c}
	}
	return n
}

// Put writes a node or an edge the engine makes for itself; Drop removes such a
// node with the edges its owner stated on it.
func (s *Session) Put(n *Node)     { s.ix.state.Nodes[n.ID] = n }
func (s *Session) PutEdge(e *Edge) { s.ix.state.Edges[e.key()] = e }
func (s *Session) Drop(id string)  { s.ix.deleteNode(id) }

// ChunkHash is the vector key of a text embedded on its own, with no prefix.
func ChunkHash(text string) string { return sum(text) }
