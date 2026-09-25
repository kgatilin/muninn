package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
)

func init() {
	commands = append(commands, nodeCmd, nodesCmd)
}

type jsonNode struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind,omitempty"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	At        *time.Time        `json:"at,omitempty"`
	Connector string            `json:"connector,omitempty"`
	Chunks    int               `json:"chunks"`
	Text      string            `json:"text,omitempty"`
	Neighbors []jsonNeighbor    `json:"neighbors,omitempty"`
}

type jsonNeighbor struct {
	// Dir is "out" for an edge from the node, "in" for one to it.
	Dir    string  `json:"dir"`
	Edge   string  `json:"edge"`
	ID     string  `json:"id"`
	Kind   string  `json:"kind,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

func describe(n *index.Node, withText bool) jsonNode {
	out := jsonNode{ID: n.ID, Kind: n.Kind, Attrs: n.Attrs, Connector: n.Connector, Chunks: len(n.Chunks)}
	if !n.At.IsZero() {
		out.At = &n.At
	}
	if withText {
		texts := make([]string, len(n.Chunks))
		for i, c := range n.Chunks {
			texts[i] = c.Text
		}
		out.Text = strings.Join(texts, "\n")
	}
	return out
}

// neighbors are the node's edges, outgoing first, then by edge kind and id. An
// edge may point at a node the bank does not hold; its kind is then empty.
func neighbors(state *index.State, id string) []jsonNeighbor {
	var out []jsonNeighbor
	for _, e := range state.Edges {
		nb := jsonNeighbor{Edge: e.Kind, Weight: e.Weight}
		switch id {
		case e.From:
			nb.Dir, nb.ID = "out", e.To
		case e.To:
			nb.Dir, nb.ID = "in", e.From
		default:
			continue
		}
		if n := state.Nodes[nb.ID]; n != nil {
			nb.Kind = n.Kind
		}
		out = append(out, nb)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Dir != b.Dir {
			return a.Dir > b.Dir
		}
		if a.Edge != b.Edge {
			return a.Edge < b.Edge
		}
		return a.ID < b.ID
	})
	return out
}

func attrLine(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + attrs[k]
	}
	return strings.Join(keys, " ")
}

// locatorRE is a hit as `search` prints it: a node's id and the lines of a chunk.
var locatorRE = regexp.MustCompile(`^(.+):(\d+)(?:-(\d+))?$`)

func chunkLines(c index.Chunk) string {
	if c.EndLine <= c.Line {
		return fmt.Sprintf(":%d", c.Line)
	}
	return fmt.Sprintf(":%d-%d", c.Line, c.EndLine)
}

func nodeCmd() *cobra.Command {
	var withNeighbors, asJSON bool
	cmd := &cobra.Command{
		Use:   "node <bank> <id>",
		Short: "A node: what it is, its text, and with --neighbors its edges",
		Long: `A node: what it is, its text, and with --neighbors its edges.

With a locator as ` + "`search`" + ` prints it, the id and the lines, only the chunks
that hold those lines:

  muninn node notes /Users/me/notes/agents/memory.md:42-67`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			state, err := index.LoadState(b.IndexDir())
			if err != nil {
				return err
			}
			n := state.Nodes[args[1]]
			if m := locatorRE.FindStringSubmatch(args[1]); n == nil && m != nil && state.Nodes[m[1]] != nil {
				first, _ := strconv.Atoi(m[2])
				last := first
				if m[3] != "" {
					last, _ = strconv.Atoi(m[3])
				}
				for _, c := range state.Nodes[m[1]].Chunks {
					if c.Line <= last && c.EndLine >= first {
						fmt.Printf("%s%s\n%s\n", m[1], chunkLines(c), c.Text)
					}
				}
				return nil
			}
			if n == nil {
				return fmt.Errorf("bank %q has no node %q; `muninn nodes %s` lists them", b.Name, args[1], b.Name)
			}
			d := describe(n, true)
			if withNeighbors {
				d.Neighbors = neighbors(state, n.ID)
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				return enc.Encode(d)
			}

			fmt.Printf("%s [%s]\n", d.ID, d.Kind)
			if len(d.Attrs) > 0 {
				fmt.Println("attrs", attrLine(d.Attrs))
			}
			if d.At != nil {
				fmt.Println("at", d.At.Format(time.RFC3339))
			}
			fmt.Printf("connector %s, %d chunks\n", d.Connector, d.Chunks)
			group := ""
			for _, nb := range d.Neighbors {
				if g := nb.Dir + " " + nb.Edge; g != group {
					group = g
					fmt.Println(g)
				}
				fmt.Printf("  %s [%s]\n", nb.ID, nb.Kind)
			}
			if d.Text != "" {
				fmt.Printf("text\n%s\n", d.Text)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&withNeighbors, "neighbors", false, "list the node's edges, grouped by direction and kind")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON instead of text")
	return cmd
}

func nodesCmd() *cobra.Command {
	var kind string
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "nodes <bank>",
		Short:             "List a bank's nodes: id, kind, chunk count",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			state, err := index.LoadState(b.IndexDir())
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(state.Nodes))
			for id, n := range state.Nodes {
				if kind == "" || n.Kind == kind {
					ids = append(ids, id)
				}
			}
			sort.Strings(ids)
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			for _, id := range ids {
				n := state.Nodes[id]
				if asJSON {
					if err := enc.Encode(describe(n, false)); err != nil {
						return err
					}
					continue
				}
				fmt.Printf("%s\t%s\t%d\n", n.ID, n.Kind, len(n.Chunks))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "only nodes of this kind")
	cmd.Flags().BoolVar(&asJSON, "json", false, "one JSON object per line, with attrs")
	return cmd
}
