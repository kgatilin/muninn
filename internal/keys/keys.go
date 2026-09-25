// Package keys is the stage that ties nodes by what their ids share: a bank
// names a pattern — a ticket number — and every node whose id holds a match is
// tied to the one node that match becomes. A design folder and the branch the
// design was built on carry the same ticket, and through it a rule drawn from
// the design reaches the sessions of the branch and the files they touched.
//
// It reads ids and nothing else, asks no model, and is worked out whole at
// every run: what it owns and would not state now is dropped.
package keys

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
)

// EdgeHas is node → key node: the node's id holds the key.
const EdgeHas = "has"

const ownerPrefix = "~key:"

// Stage runs for a bank with no keys too: the last pattern taken away leaves
// nodes to remove.
func Stage(b *bank.Bank) func(context.Context, *index.Session) error {
	return func(ctx context.Context, s *index.Session) error {
		if len(b.Keys) == 0 && !owns(s.State()) {
			return nil
		}
		tied := Run(s)
		names := make([]string, 0, len(tied))
		for name := range tied {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(s.Log(), "keys %s: %d keys tie %d nodes\n", name, tied[name][0], tied[name][1])
		}
		_, err := s.Commit(ctx)
		return err
	}
}

func owns(state *index.State) bool {
	for _, n := range state.Nodes {
		if strings.HasPrefix(n.Connector, ownerPrefix) {
			return true
		}
	}
	return false
}

// Run states the bank's keys over the graph as it stands and returns, by
// name, how many keys tie how many nodes. A key one node holds ties nothing
// and is not made.
func Run(s *index.Session) map[string][2]int {
	state := s.State()
	wantNodes, wantEdges := map[string]bool{}, map[string]bool{}
	out := map[string][2]int{}
	for name, pattern := range s.Bank().Keys {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		owner := ownerPrefix + name
		holders := map[string][]string{}
		for id, n := range state.Nodes {
			if strings.HasPrefix(n.Connector, "~") {
				continue
			}
			seen := map[string]bool{}
			for _, m := range re.FindAllString(id, -1) {
				if !seen[m] {
					seen[m] = true
					holders[m] = append(holders[m], id)
				}
			}
		}
		var st [2]int
		for match, ids := range holders {
			if len(ids) < 2 {
				continue
			}
			keyID := name + ":" + match
			if old := state.Nodes[keyID]; old != nil && old.Connector != owner {
				continue
			}
			wantNodes[keyID] = true
			if state.Nodes[keyID] == nil {
				s.Put(index.NewTextNode(keyID, name, owner, match, nil))
			}
			st[0]++
			for _, id := range ids {
				e := &index.Edge{From: id, To: keyID, Kind: EdgeHas, Weight: 1, Connector: owner}
				wantEdges[e.Key()] = true
				s.PutEdge(e)
				st[1]++
			}
		}
		out[name] = st
	}
	for k, e := range state.Edges {
		if strings.HasPrefix(e.Connector, ownerPrefix) && !wantEdges[k] {
			delete(state.Edges, k)
		}
	}
	for id, n := range state.Nodes {
		if strings.HasPrefix(n.Connector, ownerPrefix) && !wantNodes[id] {
			s.Drop(id)
		}
	}
	return out
}
