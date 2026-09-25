package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/usage"
)

func init() { commands = append(commands, usageCmd) }

func usageCmd() *cobra.Command {
	var (
		by    string
		since string
	)
	cmd := &cobra.Command{
		Use:   "usage [bank]",
		Short: "What the paid model calls came to, from the usage log of the muninn home",
		Long: `What the paid model calls came to: embeddings and chat completions of hosted
providers, one line per call in usage.jsonl under the muninn home. The log holds
tokens; the cost is worked out from the price table as it stands now. Local
models are not recorded. Failed requests are not counted, and neither are
cached-token or batch discounts: the figure is an estimate, not the bill.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: bankNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := bank.Home()
			if err != nil {
				return err
			}
			var from time.Time
			if since != "" {
				if from, err = time.ParseInLocation("2006-01-02", since, time.Local); err != nil {
					return fmt.Errorf("--since is a date, 2006-01-02: %w", err)
				}
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			if by == "" {
				by = "purpose"
				if name == "" {
					by = "bank"
				}
			}
			recs, err := usage.Read(home)
			if err != nil {
				return err
			}
			prices, err := usage.Prices(home)
			if err != nil {
				return err
			}
			lines, total, err := usage.Summarize(usage.Filter(recs, name, from), prices, by)
			if err != nil {
				return err
			}
			if total.Calls == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no paid calls recorded")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', tabwriter.AlignRight)
			fmt.Fprintf(tw, "%s\tcalls\trequests\ttokens in\ttokens out\tcost\t\n", by)
			for _, l := range append(lines, total) {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t\n", l.Key, l.Calls, l.Requests, l.In, l.Out, cost(l))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "sum by bank, model, purpose or day (default: purpose for a bank, bank otherwise)")
	cmd.Flags().StringVar(&since, "since", "", "only calls since this date, 2006-01-02")

	price := &cobra.Command{
		Use:   "price [<provider:model> <$ per 1M in> [<$ per 1M out>]]",
		Short: "Show the price table, or state a model's price",
		Args:  cobra.MaximumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := bank.Home()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				prices, err := usage.Prices(home)
				if err != nil {
					return err
				}
				models := make([]string, 0, len(prices))
				for m := range prices {
					models = append(models, m)
				}
				sort.Strings(models)
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "model\t$ per 1M in\t$ per 1M out")
				for _, m := range models {
					fmt.Fprintf(tw, "%s\t%g\t%g\n", m, prices[m].In, prices[m].Out)
				}
				return tw.Flush()
			}
			if len(args) == 1 {
				return errors.New("a price is the model, dollars per million tokens in and, for a chat model, out")
			}
			var p usage.Price
			if p.In, err = strconv.ParseFloat(args[1], 64); err != nil || p.In < 0 {
				return fmt.Errorf("%q is not a price", args[1])
			}
			if len(args) == 3 {
				if p.Out, err = strconv.ParseFloat(args[2], 64); err != nil || p.Out < 0 {
					return fmt.Errorf("%q is not a price", args[2])
				}
			}
			return usage.SetPrice(home, args[0], p)
		},
	}
	cmd.AddCommand(price)
	return cmd
}

// cost is dollars to the cent of a cent; calls to a model with no price are
// said beside it, since the figure leaves them out.
func cost(l usage.Line) string {
	s := fmt.Sprintf("$%.4f", l.Cost)
	if l.Unpriced > 0 {
		s += fmt.Sprintf(" (+%d unpriced)", l.Unpriced)
	}
	return s
}
