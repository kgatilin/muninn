package index

import (
	"sort"
	"strings"
	"time"
)

// EdgeIn is member → container: a message in its session. Compaction reads it
// to know which node stays for the ones that go.
const EdgeIn = "in"

// Compact removes the nodes of the bank's compact.kinds whose container is old
// and read: every such node `in` it is older than compact.after and read says
// every stage has been over it. A container goes whole or not at all, so a
// conversation is never left with holes.
//
// What the members were tied to outside the container — the items they state,
// the entities they mention, the files they touched — the container is tied
// to from then on, the weights of edges that fall together summed. The ids are
// kept in State.Compacted, and a connector that emits them again is not heard.
func (s *Session) Compact(now time.Time, read func(*Node) bool) (nodes, containers int) {
	cfg := s.ix.bank.Compact
	if !cfg.On() {
		return 0, 0
	}
	state := s.ix.state
	cutoff := now.AddDate(0, 0, -cfg.Days)
	members := map[string][]*Node{}
	kept := map[string]bool{}
	for _, e := range state.Edges {
		n := state.Nodes[e.From]
		if e.Kind != EdgeIn || n == nil || !cfg.Compacts(n.Kind) || state.Nodes[e.To] == nil {
			continue
		}
		members[e.To] = append(members[e.To], n)
		if n.At.IsZero() || n.At.After(cutoff) || !read(n) {
			kept[e.To] = true
		}
	}
	home := map[string]string{}
	for container, ms := range members {
		if kept[container] {
			continue
		}
		containers++
		for _, n := range ms {
			home[n.ID] = container
		}
	}
	if len(home) == 0 {
		return 0, 0
	}

	var keys []string
	for k, e := range state.Edges {
		if home[e.From] != "" || home[e.To] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := state.Edges[k]
		delete(state.Edges, k)
		moved := &Edge{From: e.From, To: e.To, Kind: e.Kind, Weight: e.Weight, Connector: e.Connector}
		if c := home[e.From]; c != "" {
			moved.From = c
		}
		if c := home[e.To]; c != "" {
			moved.To = c
		}
		// An edge between two members, or from a member to its container, said
		// something of the conversation's shape only.
		if moved.From == moved.To {
			continue
		}
		if old := state.Edges[moved.key()]; old != nil {
			old.Weight += moved.Weight
			continue
		}
		state.Edges[moved.key()] = moved
	}

	if state.Compacted == nil {
		state.Compacted = map[string]string{}
	}
	for id := range home {
		state.Compacted[id] = state.Nodes[id].Connector
		delete(state.Nodes, id)
		delete(state.Semantic, id)
		delete(state.Named, id)
	}
	for k := range state.Enriched {
		if _, id, ok := strings.Cut(k, "\x00"); ok && home[id] != "" {
			delete(state.Enriched, k)
		}
	}
	return len(home), containers
}
