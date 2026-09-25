package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/export"
	"github.com/kgatilin/muninn/internal/index"
)

func init() {
	commands = append(commands, exportCmd)
}

func exportCmd() *cobra.Command {
	var (
		graphml bool
		out     string
		o       export.Options
	)
	cmd := &cobra.Command{
		Use:   "export <bank> --graphml [-o file]",
		Short: "Write a bank's graph as GraphML, for archmotif or Gephi",
		Long: `Write a bank's graph as GraphML, in the dialect archmotif reads. A node
carries id, label, kind, connector, chunks, degree and at; an edge kind, weight
and connector. An edge to a node the bank does not hold is left out, and stderr
says how many were.`,
		Example: `  muninn export work --graphml -o work.graphml && archmotif analyze work.graphml
  muninn export work --graphml --kind entity --kind turn --edge-kind mentions`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if !graphml {
				return errors.New("--graphml is the one format there is; state it")
			}
			b, err := bank.Load(args[0])
			if err != nil {
				return err
			}
			state, err := index.LoadState(b.IndexDir())
			if err != nil {
				return err
			}
			var w io.Writer = os.Stdout
			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			res, err := export.GraphML(w, state, o)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%d nodes, %d edges", res.Nodes, res.Edges)
			if res.Dangling > 0 {
				fmt.Fprintf(os.Stderr, "; %d edges left out, an end is not a node of the bank", res.Dangling)
			}
			fmt.Fprintln(os.Stderr)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&graphml, "graphml", false, "GraphML")
	f.StringVarP(&out, "output", "o", "", "the file to write (default: stdout)")
	f.StringArrayVar(&o.Kinds, "kind", nil, "node kinds to write (default: all)")
	f.StringArrayVar(&o.EdgeKinds, "edge-kind", nil, "edge kinds to write (default: all)")
	return cmd
}
