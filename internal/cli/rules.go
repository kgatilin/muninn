package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/enrich"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/internal/ui"
)

func init() {
	commands = append(commands, rulesCmd)
}

// showEnrich is the enrichments' part of `bank show`.
func showEnrich(b *bank.Bank, state *index.State) {
	for _, st := range enrich.Status(b, state) {
		name := st.Name
		e := b.Enrich.Sets[name].Resolved()
		fmt.Printf("enrich %s", name)
		if missing := b.Enrich.Sets[name].Missing(); missing != "" {
			fmt.Printf(" (left out: it needs %s)", missing)
		}
		if b.Enrich.Sets[name].Preset != "" {
			fmt.Printf(" preset=%s", b.Enrich.Sets[name].Preset)
		}
		if len(e.Kinds) > 0 {
			fmt.Printf(" kinds=%s", strings.Join(e.Kinds, ","))
		}
		if len(e.Paths) > 0 {
			fmt.Printf(" paths=%s", strings.Join(e.Paths, ","))
		}
		if len(e.Fields) > 0 {
			fmt.Printf(" fields=%s", strings.Join(e.Fields, ","))
		}
		fmt.Printf(" kind=%s neighbours=%d votes=%d ratio=%g model=%s\n  %d items, %d nodes read, %d pending\n", e.Kind, e.Neighbours, e.Votes, e.Ratio, orNone(b.Enrich.Model), st.Items, st.Read, st.Pending)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none: `bank set <bank> enrich.model=…`)"
	}
	return s
}

func rulesCmd() *cobra.Command {
	var k int
	var reset bool
	var of string
	var halfLife int
	var bySources bool
	cmd := &cobra.Command{
		Use:   "rules <bank> [query]",
		Short: "What the bank's enrichments drew from it: all of it, or what is closest to a query",
		Long: `What the bank's enrichments drew from its nodes: the rules a user gave the
agents, out of the conversation turns; the rules of the architecture, out of
the design documents; whatever else the bank's enrich.* settings ask for. A
later passage that says otherwise rewrites the item it contradicts, so each
line is what holds now.

With a query, the items closest to it:

  muninn rules app "commit message for a change to the parser"
  muninn rules app --type architecture "a new storage backend"

Ask before starting a piece of work, with the ticket, the branch and what is
about to be done as the query. Among the items that match, the later stated
rank higher: a score halves for every --half-life days of age. Without a query,
every item, the most recently stated first, or with --by-sources the ones most
passages stand behind: what the bank holds to most widely. ` + "`muninn node <bank> <id> --neighbors`" + ` shows the passages behind one.

The enrichments run in ` + "`index`" + `. --reset drops the items, of one enrichment
with --type, and the record of the nodes read; the next index reads them again.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			if _, ok := b.Enrich.Sets[of]; of != "" && !ok && !reset {
				return fmt.Errorf("bank %q has no enrichment %q", b.Name, of)
			}
			if reset {
				s, err := index.Open(b, os.Stderr)
				if err != nil {
					return err
				}
				defer s.Close()
				n := enrich.Reset(s.State(), of)
				if _, err := s.Commit(cmd.Context()); err != nil {
					return err
				}
				fmt.Printf("%d items of %s dropped\n", n, b.Name)
				return nil
			}
			if len(args) > 1 {
				query := strings.Join(args[1:], " ")
				o := search.Options{K: k, Filters: map[string]string{}, Owner: "~enrich:", HalfLife: time.Duration(halfLife) * 24 * time.Hour}
				if of != "" {
					o.Filters["type"] = of
				}
				r := search.Render{Full: true, Attrs: []string{"type"}}
				if ok, err := askServer(cmd.Context(), b.Name, ui.Query{Text: query, Options: o, Render: r}, os.Stdout); ok {
					return err
				}
				hits, warning, err := search.Run(cmd.Context(), b, query, o)
				if err != nil {
					return err
				}
				r.Warning = warning
				r.Text(os.Stdout, query, hits)
				return nil
			}
			state, err := index.LoadState(b.IndexDir())
			if err != nil {
				return err
			}
			var all []*index.Node
			for _, n := range state.Nodes {
				if name := index.Enrichment(n); name != "" && (of == "" || name == of) {
					all = append(all, n)
				}
			}
			sources := map[string]int{}
			for _, e := range state.Edges {
				if to := state.Nodes[e.To]; to != nil && index.Enrichment(to) != "" && e.Connector == to.Connector {
					sources[e.To]++
				}
			}
			sort.Slice(all, func(i, j int) bool {
				if a, b := sources[all[i].ID], sources[all[j].ID]; bySources && a != b {
					return a > b
				}
				if !all[i].At.Equal(all[j].At) {
					return all[i].At.After(all[j].At)
				}
				return all[i].ID < all[j].ID
			})
			if len(all) == 0 {
				if len(b.Enrich.Sets) == 0 || b.Enrich.Model == "" {
					return fmt.Errorf("bank %q has no enrichment; `muninn bank set %s enrich.model=<provider>[:<model>]` and `enrich.<name>.preset=%s`, then `muninn index %s`", b.Name, b.Name, strings.Join(bank.EnrichPresets(), "|"), b.Name)
				}
				fmt.Println("nothing yet")
				return nil
			}
			if cmd.Flags().Changed("k") {
				all = all[:min(len(all), k)]
			}
			for _, n := range all {
				fmt.Printf("%s · %s · %s · %d sources\n  %s\n", n.ID, n.Attrs["type"], n.At.Local().Format("2006-01-02"), sources[n.ID], strings.ReplaceAll(n.Chunks[0].Text, "\n", "\n  "))
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&k, "k", "k", 5, "how many items a query returns; given without a query, how many are listed")
	cmd.Flags().StringVar(&of, "type", "", "only the items of this enrichment")
	cmd.Flags().IntVar(&halfLife, "half-life", 90, "days of age over which an item's score halves; 0 ranks by the match alone")
	cmd.Flags().BoolVar(&bySources, "by-sources", false, "without a query: the items most nodes state first")
	cmd.Flags().BoolVar(&reset, "reset", false, "drop the items, to be drawn again by the next index")
	return cmd
}
