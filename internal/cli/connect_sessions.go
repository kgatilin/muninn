package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/sessconn"
	"github.com/kgatilin/muninn/stream"
)

func init() {
	connectors = append(connectors,
		func() *cobra.Command {
			return sessionsCmd("claude", "Claude Code sessions as turns, with the files each turn read and edited")
		},
		func() *cobra.Command {
			return sessionsCmd("codex", "Codex sessions as turns, with the files each turn patched")
		},
	)
}

func sessionsCmd(tool, short string) *cobra.Command {
	o := sessconn.Options{Tool: tool}
	cmd := &cobra.Command{
		Use:   tool,
		Short: short,
		Long: short + `.

A turn is the user's message and the agent's text replies up to the next user
message. Tool calls, tool results, reasoning and injected context are left out
of the text; the files the calls touched become edges to the file's absolute
path, the id ` + "`connect fs`" + ` gives the same file.

The cursor is the newest modification time among the session files read. A run
reads the files modified since, and writes each of those sessions whole.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			o.Cursor = os.Getenv("MUNINN_CURSOR")
			return sessconn.Run(stream.NewWriter(os.Stdout), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Root, "root", "", "the session tree (default "+sessconn.DefaultRoot(tool)+")")
	f.StringArrayVar(&o.Projects, "project", nil, `only sessions of this project; repeatable. A path takes sessions whose
directory is the path or under it. A name (no slash) takes sessions whose
directory has the name as a path segment. A worktree counts as its repo:
one kept inside it (<repo>/.worktrees/<name>) matches by its directory, one
kept elsewhere matches by the main repo its .git file names`)
	return cmd
}
