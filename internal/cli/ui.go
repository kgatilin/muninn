package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/ui"
)

func init() {
	commands = append(commands, uiCmd)
}

// DefaultAddr is where `up` listens and where `search` looks for it.
const DefaultAddr = "127.0.0.1:7811"

// serverAddr is the address of the running server as a command sees it.
func serverAddr() string {
	if v := os.Getenv("MUNINN_ADDR"); v != "" {
		return v
	}
	return DefaultAddr
}

func uiCmd() *cobra.Command {
	var addr string
	var open bool
	var every time.Duration
	cmd := &cobra.Command{
		Use:     "up",
		Aliases: []string{"ui"},
		Short:   "Keep every bank loaded: answer `muninn search`, serve the local page, index on a period",
		Long: `Keep every bank loaded in one process. ` + "`muninn search`" + ` asks it first and
loads the bank itself only when it is not there, so a query against a large
bank does not pay for reading it. The same process serves the local page: each
bank's summary, its graph drawn, a node's text and neighbours on selection, and
search with the hits lit on the graph.

A bank that sets index.every (` + "`muninn bank set <bank> index.every=15m`" + `) is
indexed on that period, as ` + "`muninn index`" + ` would, and loaded again once its
run commits; --index-every is the period of the banks that set none. A run
with nothing new makes no paid call. With neither, the process reads the
committed snapshots and writes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			url := "http://" + ln.Addr().String() + "/"
			fmt.Fprintln(os.Stderr, "muninn ui on", url)
			if open {
				openBrowser(url)
			}
			server := ui.NewServer()
			go func() {
				server.Warm(os.Stderr)
				indexEvery(cmd.Context(), server, every)
			}()
			srv := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-cmd.Context().Done()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				srv.Shutdown(ctx)
			}()
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", serverAddr(), "address to listen on; MUNINN_ADDR sets it for every command")
	cmd.Flags().DurationVar(&every, "index-every", 0, "the period for the banks that set no index.every of their own, e.g. 15m")
	cmd.Flags().BoolVar(&open, "open", false, "open the page in the browser")
	return cmd
}

// indexEvery looks the banks over once a minute and indexes, one after
// another, those whose period has passed since their last run: the bank's
// index.every, or fallback where it sets none. The settings are read at every
// look, so a change needs no restart. A run that fails is logged and the next
// bank still runs: an expired token should not stop the banks that need none.
// It is tried again after its period, not at the next look.
func indexEvery(ctx context.Context, server *ui.Server, fallback time.Duration) {
	tried := map[string]time.Time{}
	for {
		names, err := bank.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "muninn:", err)
		}
		ran := false
		for _, name := range names {
			if ctx.Err() != nil {
				return
			}
			b, err := bank.Load(name)
			if err != nil {
				continue
			}
			every := b.Every()
			if every == 0 {
				every = fallback
			}
			last := server.LastRun(name)
			if tried[name].After(last) {
				last = tried[name]
			}
			if every == 0 || time.Since(last) < every {
				continue
			}
			tried[name], ran = time.Now(), true
			if err := runIndex(ctx, b, index.Options{}); err != nil {
				fmt.Fprintf(os.Stderr, "muninn: index %s: %v\n", name, err)
			}
		}
		if ran {
			server.Warm(io.Discard)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// askServer runs a search on the server at serverAddr. ok is false when no
// server answered there, and the command then loads the bank itself.
func askServer(ctx context.Context, name string, q ui.Query, out io.Writer) (ok bool, err error) {
	conn, err := net.DialTimeout("tcp", serverAddr(), 200*time.Millisecond)
	if err != nil {
		return false, nil
	}
	conn.Close()
	u := url.URL{Scheme: "http", Host: serverAddr(), Path: "/api/banks/" + name + "/query", RawQuery: q.Values().Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	// Anything but the endpoint's own answers is some other program on the
	// port, or a server older than the endpoint.
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		var e struct{ Error string }
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Error == "" {
			return false, nil
		}
		return true, errors.New(e.Error)
	default:
		return false, nil
	}
	_, err = io.Copy(out, resp.Body)
	return true, err
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if err := exec.Command(name, url).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "muninn: could not open the browser:", err)
	}
}
