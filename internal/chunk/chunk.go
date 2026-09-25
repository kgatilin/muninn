// Package chunk holds the chunking strategies: named, versioned, and chosen per
// node or per bank. Chunks are internal to the index and never graph nodes.
package chunk

import (
	"fmt"
	"sort"
)

// Chunk is a byte span of the node's text. Prefix is context the span does not
// carry itself — a heading path — and is embedded and indexed with it.
type Chunk struct {
	Start, End int
	Prefix     string
}

// Params are per chunker, per bank.
type Params struct {
	// Budget is the target chunk size in runes.
	Budget int `yaml:"budget,omitempty"`
}

const DefaultBudget = 1600

type Chunker interface {
	Name() string
	// Version changes when the same text would chunk differently.
	Version() int
	Chunk(text string, p Params) []Chunk
}

var registry = map[string]Chunker{}

func register(c Chunker) { registry[c.Name()] = c }

func Get(name string) (Chunker, bool) {
	c, ok := registry[name]
	return c, ok
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Recipe identifies how a node was chunked; a different recipe re-chunks.
func Recipe(c Chunker, p Params) string {
	return fmt.Sprintf("%s@%d/%d", c.Name(), c.Version(), budget(p))
}

func budget(p Params) int {
	if p.Budget <= 0 {
		return DefaultBudget
	}
	return p.Budget
}

func init() {
	register(none{})
	register(text{})
}

// none makes the node one chunk. It is also how a connector chunks for itself:
// small nodes, each left whole.
type none struct{}

func (none) Name() string { return "none" }
func (none) Version() int { return 1 }
func (none) Chunk(s string, _ Params) []Chunk {
	if s == "" {
		return nil
	}
	return []Chunk{{Start: 0, End: len(s)}}
}
