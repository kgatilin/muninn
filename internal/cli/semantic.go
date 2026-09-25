package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/semantic"
)

func init() {
	commands = append(commands, semanticCmd, graphCmd)
}

func semanticCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "semantic", Short: "The semantic layer: entities, topics and mentions the engine builds behind a gate"}

	reset := &cobra.Command{
		Use:               "reset <bank>",
		Short:             "Drop the whole layer, to be built again by the next index",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			s, err := index.Open(b, os.Stderr)
			if err != nil {
				return err
			}
			defer s.Close()
			nodes, edges := semantic.Reset(s.State())
			if _, err := s.Commit(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("semantic layer of %s dropped: %d nodes, %d edges; the evidence log stays\n", b.Name, nodes, edges)
			return nil
		},
	}

	var rejected bool
	var n int
	log := &cobra.Command{
		Use:   "log <bank>",
		Short: "The tail of the evidence log: every proposal, what the checks measured, what was decided",
		Long: `The tail of the evidence log, index/semantic.log. One proposal per entry:

  09-20 14:02:11 reject   r1 notes/agents.md  add_entity×2 add_mention×5: event log, …  region 412
    fractal reject d_B 3.10 → 3.62 loss 0 → 0 …

Under it, one line per check that did not simply accept. "dropped" is the last
rejection of a text node: nothing of that patch is in the layer.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			records, err := semantic.ReadJournal(b.IndexDir(), n, rejected)
			if err != nil {
				return err
			}
			for _, ev := range records {
				fmt.Printf("%s %-7s r%d %s  %s  region %d", ev.Time.Local().Format("01-02 15:04:05"), ev.Decision, ev.Round, ev.Node, ev.Ops, ev.Region.Nodes)
				if ev.Region.Truncated {
					fmt.Print(" (cut at the cap)")
				}
				fmt.Println()
				if ev.Error != "" {
					fmt.Println("  error:", ev.Error)
				}
				for _, r := range ev.Checks {
					if r.Decision == semantic.Accept && !rejected {
						continue
					}
					fmt.Printf("  %s %s %s → %s", r.Check, r.Decision, measured(r.Before), measured(r.After))
					if r.Decision != semantic.NotApplicable {
						fmt.Printf(" loss %.4g → %.4g", r.LossBefore, r.LossAfter)
					}
					if r.Reason != "" && r.Decision == semantic.Reject {
						fmt.Printf("  %s", r.Reason)
					}
					fmt.Println()
				}
			}
			return nil
		},
	}
	log.Flags().BoolVar(&rejected, "rejected", false, "only proposals that did not stay, with every check's evidence")
	log.Flags().IntVarP(&n, "n", "n", 20, "how many entries; 0 is all of them")

	cmd.AddCommand(reset, log)
	return cmd
}

func measured(m semantic.Measurement) string {
	if m.Status != semantic.StatusOK {
		return m.Status
	}
	return fmt.Sprintf("%.4g", m.Value)
}

func metricLine(name string, m semantic.GraphMetrics) string {
	sf, fr := m.ScaleFree, m.Fractal
	line := fmt.Sprintf("%s %d nodes, %d edges: γ %s", name, m.Nodes, m.Edges, measured(sf))
	if sf.Status == semantic.StatusOK {
		line += fmt.Sprintf(" (k_min %g, tail %g, KS %.3f, R_ln %.2f)", sf.Details["k_min"], sf.Details["tail"], sf.Details["ks"], sf.Details["r_ln"])
	}
	line += fmt.Sprintf(", d_B %s", measured(fr))
	if fr.Details != nil {
		line += fmt.Sprintf(" (%g radii, %g centres)", fr.Details["radii"], fr.Details["centres"])
	}
	return line
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func kindCounts(m map[string]int) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		name := k
		if name == "" {
			name = "(none)"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, m[k]))
	}
	return strings.Join(parts, ", ")
}

func hubLine(hubs []semantic.Hub, withKind bool) string {
	parts := make([]string, len(hubs))
	for i, h := range hubs {
		parts[i] = fmt.Sprintf("%s %d", h.ID, h.Degree)
		if h.Reach > 0 {
			parts[i] = fmt.Sprintf("%s reach %d", h.ID, h.Reach)
		}
		if withKind {
			parts[i] += " [" + h.Kind + "]"
		}
	}
	return strings.Join(parts, "; ")
}

func graphCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "graph", Short: "The bank's graph as a whole: what it looks like, and how it sits against the bank's targets"}

	load := func(name string) (*bank.Bank, *index.State, error) {
		b, err := bank.Load(name)
		if err != nil {
			return nil, nil, err
		}
		state, err := index.LoadState(b.IndexDir())
		return b, state, err
	}

	var asJSON bool
	var top int
	stats := &cobra.Command{
		Use:   "stats <bank>",
		Short: "Nodes and edges per kind, components, degrees and hubs per node kind, and the two metrics over the whole graph",
		Long: `Whole-bank diagnostics. The two metrics — the power-law exponent γ of the
degrees with its fit distance KS and the power-law/lognormal ratio R_ln, and
the ball-growth dimension d_B — are computed over the full graph and over the
semantic graph: text nodes, entities, topics and the layer's own edges, which
is the graph the gate measures regions of. A metric that does not apply says
why: too_small, no_fit, saturated.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			started := time.Now()
			b, state, err := load(args[0])
			if err != nil {
				return err
			}
			d := semantic.Diagnose(state, b.Semantic, top)
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				return enc.Encode(d)
			}
			fmt.Println("nodes", kindCounts(d.Nodes))
			fmt.Println("edges", kindCounts(d.Edges))
			c := d.Components
			fmt.Printf("components %d, largest %d nodes (%.1f%%), %d isolated\n", c.Count, c.Largest, 100*c.LargestShare, c.Isolated)
			for _, kind := range sortedKeys(d.Degrees) {
				s := d.Degrees[kind]
				name := kind
				if name == "" {
					name = "(none)"
				}
				fmt.Printf("degree %s: %d nodes, mean %.1f, median %d, p90 %d, max %d\n", name, s.Nodes, s.Mean, s.Median, s.P90, s.Max)
				if len(s.Hubs) > 0 {
					fmt.Println("  hubs", hubLine(s.Hubs, false))
				}
			}
			fmt.Println(metricLine("full graph", d.Full))
			fmt.Println(metricLine("semantic graph", d.Semantic))
			fmt.Fprintf(os.Stderr, "measured in %s\n", time.Since(started).Round(time.Millisecond))
			return nil
		},
	}
	stats.Flags().BoolVar(&asJSON, "json", false, "JSON instead of text")
	stats.Flags().IntVar(&top, "top", 5, "how many hubs per node kind")

	var checkTop int
	var checkJSON bool
	check := &cobra.Command{
		Use:   "check <bank>",
		Short: "The whole-graph metrics against the bank's bands, and the nodes behind each miss",
		Long: `Sets the two metrics, over the semantic graph and over the full graph, against
the bands of the bank's checks. It diagnoses and does not write: it is how the
targets get chosen and how drift is noticed.

For a scale-free miss the nodes named are the ones of highest degree. For a
fractal miss above the band, or a graph that saturates within three hops, they
are the ones with the largest two-hop reach, estimated as the sum of their
neighbours' degrees — a cheap stand-in for "whose removal would stretch the
graph most", not a measurement of it. Below the band no node is to blame.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, state, err := load(args[0])
			if err != nil {
				return err
			}
			findings := semantic.CheckBank(state, b.Semantic, checkTop)
			if checkJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				return enc.Encode(findings)
			}
			params := b.Semantic.Resolved().Checks
			for _, f := range findings {
				p := params[f.Check].Params
				fmt.Printf("%s %s %s: %s against [%g, %g]", f.Graph, f.Check, f.Verdict, measured(f.Measure), p["low"], p["high"])
				if f.Check == "scale_free" && f.Measure.Status == semantic.StatusOK {
					fmt.Printf(", KS %.3f against %g, R_ln %.2f against ±%g", f.Measure.Details["ks"], p["ks"], f.Measure.Details["r_ln"], p["rln"])
				}
				if f.Loss > 0 {
					fmt.Printf(", loss %.4g", f.Loss)
				}
				fmt.Println()
				if f.Note != "" {
					fmt.Printf("  %s\n", f.Note)
				}
				if len(f.Blame) > 0 {
					fmt.Printf("  %s\n", hubLine(f.Blame, true))
				}
			}
			return nil
		},
	}
	check.Flags().IntVar(&checkTop, "top", 5, "how many nodes to name per miss")
	check.Flags().BoolVar(&checkJSON, "json", false, "JSON instead of text")

	cmd.AddCommand(stats, check)
	return cmd
}

// showSemantic is the layer's part of `bank show`.
func showSemantic(b *bank.Bank, state *index.State) {
	showEnrich(b, state)
	for _, name := range slices.Sorted(maps.Keys(b.Keys)) {
		fmt.Printf("keys %s %s\n", name, b.Keys[name])
	}
	if b.IndexEvery != "" {
		fmt.Printf("index every %s, by `muninn up`\n", b.IndexEvery)
	}
	if b.Compact.On() {
		fmt.Printf("compact kinds=%s after=%dd\n  %d nodes compacted\n", strings.Join(b.Compact.Kinds, ","), b.Compact.Days, len(state.Compacted))
	}
	cfg := b.Semantic.Resolved()
	fmt.Printf("semantic %s", cfg.Method)
	if cfg.Model != "" {
		fmt.Printf(" model=%s", cfg.Model)
	}
	if cfg.Endpoint != "" {
		fmt.Printf(" endpoint=%s", cfg.Endpoint)
	}
	if cfg.Method == bank.SemanticLLM {
		fmt.Printf(" concurrency=%d", cfg.Concurrency)
	}
	if len(cfg.Kinds) > 0 {
		fmt.Printf(" kinds=%s", strings.Join(cfg.Kinds, ","))
	}
	fmt.Printf(" mentions=%d votes=%d rounds=%d hops=%d region_cap=%d\n", cfg.Mentions, cfg.Votes, cfg.Rounds, cfg.Hops, cfg.RegionCap)
	for _, kind := range bank.CheckKinds() {
		c := cfg.Checks[kind]
		state := "on"
		if !*c.Enabled {
			state = "off"
		}
		var params []string
		for _, p := range sortedKeys(c.Params) {
			params = append(params, fmt.Sprintf("%s=%g", p, c.Params[p]))
		}
		fmt.Printf("  check %s %s  %s\n", kind, state, strings.Join(params, " "))
	}
	st := semantic.Status(state, b.Semantic)
	fmt.Printf("  layer %d entities, %d topics, %d mentions; %d text nodes read, %d pending\n",
		st.Counts.Entities, st.Counts.Topics, st.Mentions, st.Processed, st.Pending)
}
