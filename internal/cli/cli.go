// Package cli is the command tree. Banks and connectors are managed here and
// nowhere else: bank.yaml is never edited by hand.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/enrich"
	"github.com/kgatilin/muninn/internal/fsconn"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/keys"
	"github.com/kgatilin/muninn/internal/search"
	"github.com/kgatilin/muninn/internal/semantic"
	"github.com/kgatilin/muninn/internal/ui"
	"github.com/kgatilin/muninn/stream"
)

func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "muninn:", err)
		return 1
	}
	return 0
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "muninn",
		Short:         "Index text from anywhere as a graph, and search it",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(bankCmd(), connectorCmd(), indexCmd(), searchCmd(), connectCmd())
	for _, mk := range commands {
		cmd.AddCommand(mk())
	}
	return cmd
}

// commands and connectors are what other files of this package add from their
// init: a top-level command, and a connector under `muninn connect`.
var (
	commands   []func() *cobra.Command
	connectors []func() *cobra.Command
)

func bankNames(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	names, _ := bank.List()
	return names, cobra.ShellCompDirectiveNoFileComp
}

func bankCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "bank", Short: "Manage banks"}

	var embedder, chunker string
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a bank; with no flags it is a working one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bank.New(args[0])
			if err != nil {
				return err
			}
			for key, value := range map[string]string{"embedder": embedder, "chunker": chunker} {
				if value == "" {
					continue
				}
				if _, err := b.Set(key, value); err != nil {
					return err
				}
			}
			if err := b.Save(); err != nil {
				return err
			}
			fmt.Printf("bank %s: embedder %s, chunker %s\nnext: muninn connector add %s <name> -- <command>\n", b.Name, b.Embedder, b.Chunker, b.Name)
			return nil
		},
	}
	add.Flags().StringVar(&embedder, "embedder", "", "provider[:model], one of: "+strings.Join(embed.Providers(), ", "))
	add.Flags().StringVar(&chunker, "chunker", "", "the default chunker")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List banks",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			names, err := bank.List()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Println("no banks yet — `muninn bank add <name>`")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "BANK\tEMBEDDER\tCONNECTORS\tNODES")
			for _, name := range names {
				b, err := bank.Load(name)
				if err != nil {
					fmt.Fprintf(tw, "%s\t(%v)\n", name, err)
					continue
				}
				nodes := "?"
				if s, err := index.LoadState(b.IndexDir()); err == nil {
					nodes = fmt.Sprint(len(s.Nodes))
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", name, b.Embedder, len(b.Connectors), nodes)
			}
			return tw.Flush()
		},
	}

	show := &cobra.Command{
		Use:               "show <name>",
		Short:             "A bank's settings and the state of its index",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE:              func(_ *cobra.Command, args []string) error { return showBank(args[0]) },
	}

	set := &cobra.Command{
		Use:               "set <name> <key>=<value>...",
		Short:             "Change a bank's settings",
		Long:              "Change a bank's settings. Keys:\n  " + strings.Join(bank.Keys(), "\n  "),
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			var notes []string
			for _, kv := range args[1:] {
				key, value, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("%q is not key=value", kv)
				}
				note, err := b.Set(key, value)
				if err != nil {
					return err
				}
				if note != "" {
					notes = append(notes, key+": "+note)
				}
			}
			if err := b.Save(); err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Println(n)
			}
			return nil
		},
	}

	var yes bool
	rm := &cobra.Command{
		Use:               "rm <name>",
		Short:             "Delete a bank with its index",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			if !yes && !confirm(fmt.Sprintf("delete bank %s and %s?", b.Name, b.Dir)) {
				return errors.New("not deleted")
			}
			return b.Remove()
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "do not ask")

	cmd.AddCommand(add, ls, show, set, rm)
	return cmd
}

func showBank(name string) error {
	b, err := bank.Load(name)
	if err != nil {
		return err
	}
	fmt.Printf("bank %s  %s\nembedder %s", b.Name, b.Dir, b.Embedder)
	if b.Embedder.Endpoint != "" {
		fmt.Printf(" at %s", b.Embedder.Endpoint)
	}
	fmt.Printf("\nchunker %s", b.Chunker)
	for name, p := range b.Chunkers {
		fmt.Printf("  %s.budget=%d", name, p.Budget)
	}
	fmt.Printf("\nsearch seeds=%d", b.Search.Seeds)
	for kind, w := range b.Search.EdgeWeights {
		fmt.Printf(" %s=%g", kind, w)
	}
	for kind, n := range b.Search.DegreeCap {
		fmt.Printf(" degree_cap.%s=%d", kind, n)
	}
	fmt.Println()
	for _, c := range b.Classes {
		fmt.Printf("class %s", c.Name)
		if len(c.Kinds) > 0 {
			fmt.Printf(" kinds=%s", strings.Join(c.Kinds, ","))
		}
		if len(c.Paths) > 0 {
			fmt.Printf(" paths=%s", strings.Join(c.Paths, ","))
		}
		fmt.Println()
	}
	for _, id := range b.Search.Mute {
		fmt.Printf("  muted %s\n", id)
	}

	state, err := index.LoadState(b.IndexDir())
	if err != nil {
		return err
	}
	chunks, withText := 0, 0
	perConnector := map[string]int{}
	for _, n := range state.Nodes {
		chunks += len(n.Chunks)
		if len(n.Chunks) > 0 {
			withText++
		}
		perConnector[n.Connector]++
	}
	vectors := 0
	if emb, err := embed.New(b.Embedder); err == nil {
		if v, err := index.OpenVectors(b.IndexDir(), emb.Key()); err == nil {
			vectors = v.Len()
		}
	}
	fmt.Printf("index %d nodes (%d with text), %d chunks, %d edges, %d vectors\n", len(state.Nodes), withText, chunks, len(state.Edges), vectors)
	for _, c := range b.Connectors {
		fmt.Printf("connector %s  %s\n", c.Name, strings.Join(c.Command, " "))
		line := fmt.Sprintf("  %d nodes", perConnector[c.Name])
		if run, ok := state.Runs[c.Name]; ok {
			line += fmt.Sprintf(", last run %s", run.At.Format(time.DateTime))
			if run.Err != "" {
				line += " failed: " + run.Err
			}
		} else {
			line += ", never run"
		}
		if cur := state.Cursors[c.Name]; cur != "" {
			line += ", cursor " + cur
		}
		fmt.Println(line)
	}
	showSemantic(b, state)
	return nil
}

func connectorCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connector", Short: "Manage a bank's connectors"}

	var preset string
	add := &cobra.Command{
		Use:   "add <bank> <name> -- <command> [args...]",
		Short: "Add a connector: any command that writes the graph stream to stdout",
		Long: `Add a connector: any command that writes the graph stream to stdout.

--preset <name> is the short spelling of "-- muninn connect <name>" for a
connector shipped with muninn. What follows -- is then the preset's own flags.`,
		Example: `  muninn connector add personal notes -- muninn connect fs --root ~/notes
  muninn connector add app claude --preset claude -- --project app`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			command := args[2:]
			switch dash := cmd.ArgsLenAtDash(); {
			case preset == "" && (dash != 2 || len(command) == 0):
				return errors.New("usage: muninn connector add <bank> <name> -- <command> [args...]")
			case preset != "" && dash != 2 && len(command) > 0:
				return errors.New("usage: muninn connector add <bank> <name> --preset <preset> [-- <preset flags>]")
			}
			if preset != "" {
				shipped := presets()
				if !slices.Contains(shipped, preset) {
					return fmt.Errorf("no preset %q; there are: %s", preset, strings.Join(shipped, ", "))
				}
				command = append([]string{"muninn", "connect", preset}, command...)
			}
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			b.Connectors = append(b.Connectors, bank.Connector{Name: args[1], Command: command})
			if err := b.Save(); err != nil {
				return err
			}
			fmt.Printf("connector %s added to %s: %s\nnext: muninn index %s\n", args[1], b.Name, strings.Join(command, " "), b.Name)
			return nil
		},
	}
	add.Flags().StringVar(&preset, "preset", "", "a connector shipped with muninn, one of: "+strings.Join(presets(), ", "))

	ls := &cobra.Command{
		Use:               "ls <bank>",
		Short:             "List a bank's connectors",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			for _, c := range b.Connectors {
				fmt.Printf("%s  %s\n", c.Name, strings.Join(c.Command, " "))
			}
			return nil
		},
	}

	rm := &cobra.Command{
		Use:               "rm <bank> <name>",
		Short:             "Remove a connector and the nodes it produced",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			if !b.RemoveConnector(args[1]) {
				return fmt.Errorf("bank %q has no connector %q", b.Name, args[1])
			}
			removed, err := index.RemoveConnectorNodes(b, args[1])
			if err != nil {
				return err
			}
			if err := b.Save(); err != nil {
				return err
			}
			fmt.Printf("connector %s removed with its %d nodes\n", args[1], removed)
			return nil
		},
	}

	cmd.AddCommand(add, ls, rm)
	return cmd
}

// runIndex is one `index` run: the connectors, then under the same lock the
// stages the bank has switched on. The enrichments go first, so the semantic
// stage reads the items they made as the text they are; compaction goes last,
// over what both have read.
func runIndex(ctx context.Context, b *bank.Bank, o index.Options) error {
	o.Log = os.Stderr
	stages := []func(context.Context, *index.Session) error{keys.Stage(b), enrich.Stage(b), semantic.Stage(b), compactStage(b)}
	o.After = func(ctx context.Context, s *index.Session) error {
		for _, stage := range stages {
			if stage == nil {
				continue
			}
			if err := stage(ctx, s); err != nil {
				return err
			}
		}
		return nil
	}
	return index.Run(ctx, b, o)
}

func indexCmd() *cobra.Command {
	var o index.Options
	cmd := &cobra.Command{
		Use:               "index <bank>",
		Short:             "Run the bank's connectors and bring the index up to what they emit",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			return runIndex(cmd.Context(), b, o)
		},
	}
	cmd.Flags().StringVar(&o.Connector, "connector", "", "run only this connector")
	cmd.Flags().BoolVar(&o.Full, "full", false, "forget the cursors, so a resuming connector starts over")
	return cmd
}

func searchCmd() *cobra.Command {
	var (
		o       search.Options
		r       search.Render
		filters []string
		since   string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "search <bank> <query> | search <bank> --from <node id>",
		Short: "Search a bank",
		Long: `Search a bank. The text output is meant for an agent's context window:

  4 hits under /Users/me/notes/
  # guide
  1 agents/GUIDE.md:42-67 — Agent memory › Recall · 2026-03-02
    …the window of the chunk that holds the query's terms…
    also :120-150
  # document
  ~2 agents/recall.md:1-30 — Recall · 2026-01-15
    …
  # rule
  3 rule:4f1c… · 2026-02-11 · 7 sources
    …
  # graph
  ~4 agents [directory]

The hits stand by class, each class ranked on its own and taking at most -k
places, so the many turns of a conversation do not crowd out the one guide. A
node's class is its kind unless the bank says otherwise:

  muninn bank set notes class.guide.paths='**/GUIDE.md,**/README.md'
  muninn bank set notes class.talk.kinds=message,reply

With --from and no query the walk starts from one node, and the answer is what
stands around it: for a branch, the design of its ticket, the rules its
sessions stated, the files they edited.

  muninn search app --from 'branch:/Users/me/app@feature-x'

The bank's classes are answered first, in the order they were set; a class
with no hit near the best one is left out. A date is the node's own: for a
document, the day its file was added. An item of an enrichment says how many
nodes state it.

Each hit is a locator that can be opened (id:first-last line), the heading
path it sits under, and a snippet. The top text hits seed a walk over the
bank's edges and the ranking is by where the walk ends up; ~ marks a hit that
was not a seed and is there through the graph, and a node with no text is one
line of id, kind and attrs. --no-graph ranks by text alone. --full prints the whole chunk
instead of the snippet, --json the same fields for a program.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			query := strings.Join(args[1:], " ")
			if (query == "") == (o.From == "") {
				return errors.New("a query, or --from <node id>")
			}
			o.Filters = map[string]string{}
			for _, f := range filters {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					return fmt.Errorf("--filter %q is not key=value", f)
				}
				o.Filters[k] = v
			}
			if since != "" {
				if o.Since, err = parseSince(since); err != nil {
					return err
				}
			}
			if ok, err := askServer(cmd.Context(), b.Name, ui.Query{Text: query, Options: o, Render: r, JSON: asJSON}, os.Stdout); ok {
				return err
			}
			hits, warning, err := search.Run(cmd.Context(), b, query, o)
			if err != nil {
				return err
			}
			r.Warning = warning
			if asJSON {
				return r.JSON(os.Stdout, query, hits)
			}
			r.Text(os.Stdout, query, hits)
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&o.K, "k", "k", 4, "how many hits of a class")
	f.StringArrayVar(&filters, "filter", nil, "attr=value; repeatable, all must hold")
	f.StringVar(&o.Kind, "kind", "", "only nodes of this kind")
	f.StringVar(&o.From, "from", "", "instead of a query: what stands around this node, by the walk from it alone")
	f.BoolVar(&o.NoGraph, "no-graph", false, "rank by text alone, without the walk over the bank's edges")
	f.StringVar(&since, "since", "", "only nodes at or after this: 2026-01-31, RFC 3339, or an age such as 72h or 30d")
	f.IntVar(&r.Chars, "chars", search.DefaultChars, "snippet length")
	f.BoolVar(&r.Full, "full", false, "print the whole chunk instead of a snippet")
	f.StringSliceVar(&r.Attrs, "attrs", nil, "node attributes to print with each hit")
	f.BoolVar(&asJSON, "json", false, "JSON instead of text")
	return cmd
}

func parseSince(v string) (time.Time, error) {
	for _, layout := range []string{time.DateOnly, time.RFC3339} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		v = days + "h"
		if d, err := time.ParseDuration(v); err == nil {
			return time.Now().Add(-24 * d), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q: a date, an RFC 3339 time, or an age such as 72h or 30d", v)
}

// presets are the connectors under `muninn connect`, by name.
func presets() []string {
	var names []string
	for _, c := range connectCmd().Commands() {
		names = append(names, c.Name())
	}
	return names
}

func connectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connect", Short: "The connectors shipped with muninn; each writes the graph stream to stdout"}

	var o fsconn.Options
	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Files under a directory: documents, their directories, and the links between them",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fsconn.Run(stream.NewWriter(os.Stdout), o)
		},
	}
	f := fsCmd.Flags()
	f.StringVar(&o.Root, "root", "", "the directory to walk")
	f.StringArrayVar(&o.Globs, "glob", nil, "files to take, relative to the root; ** crosses directories (default **/*.md)")
	f.StringArrayVar(&o.Excludes, "exclude", nil, "files or directories to leave out; hidden directories and node_modules always are")
	f.StringVar(&o.Chunker, "chunker", "", "the chunker to state on every document (default: the bank's)")
	f.StringArrayVar(&o.ChunkerFor, "chunker-for", nil, "`<glob>=<chunker>`: the chunker for files the glob matches, first match wins; the rest get --chunker")
	f.BoolVar(&o.FollowSymlinks, "follow-symlinks", false, "walk into linked directories and read linked files, under the path through the link")
	fsCmd.MarkFlagRequired("root")

	cmd.AddCommand(fsCmd)
	for _, mk := range connectors {
		cmd.AddCommand(mk())
	}
	return cmd
}

func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}

// compactStage runs last: a node goes only once the stages before it have read
// it, and a stage that failed has ended the run before this one.
func compactStage(b *bank.Bank) func(context.Context, *index.Session) error {
	if !b.Compact.On() {
		return nil
	}
	return func(ctx context.Context, s *index.Session) error {
		state := s.State()
		nodes, containers := s.Compact(time.Now(), func(n *index.Node) bool {
			return enrich.Read(b, state, n) && semantic.Read(b.Semantic, state, n)
		})
		if nodes == 0 {
			return nil
		}
		fmt.Fprintf(s.Log(), "compact: %d nodes of %d containers removed\n", nodes, containers)
		_, err := s.Commit(ctx)
		return err
	}
}
