package semantic

import (
	"context"

	"github.com/kgatilin/muninn/internal/index"
)

// View is what an extractor may read of the layer while it thinks. It cannot
// write: a patch through the gate is the only way in.
type View interface {
	Node(id string) *index.Node
	Degree(id string) int
	Neighbors(id string) []string
}

// Extractor proposes the semantic edges of one text node. The gate is the
// same whatever proposes.
type Extractor interface {
	// ID names the method and its version. It is stored with every node the
	// stage has read, so a different extractor reads the bank again.
	ID() string
	// Extract proposes a patch for the node; an empty patch says the node has
	// nothing worth an entity.
	Extract(ctx context.Context, node *index.Node, view View) (Patch, error)
	// Revise answers a rejection with another patch, given every violation and
	// the number of the round that failed, starting at 1. False gives up.
	Revise(ctx context.Context, node *index.Node, rejected Patch, violations []Result, round int) (Patch, bool)
}

// Finisher is an extractor with a pass over the whole layer once the text
// nodes are read and committed — the llm extractor's entity merges. Its patches
// go through the gate like any other.
type Finisher interface {
	Finish(ctx context.Context, view View) ([]Patch, error)
}

// Preparer is an extractor that looks at the whole run before its first node
// is read: it is given every pending node, in the order the stage reads them.
// The llm extractor has the model name every passage there, and counts the
// names.
type Preparer interface {
	Prepare(ctx context.Context, nodes []*index.Node, view View) error
}
