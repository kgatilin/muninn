// Package sessconn is the coding-agent session connectors: Claude Code and
// Codex sessions, read from the files the tools keep of their own work and
// written to the graph stream as messages and replies.
//
// A user's message is a node, and so is every text reply of the agent up to
// the next message: one message may set off hours of work, and the replies are
// where that work is told. Tool calls, tool results, reasoning and injected
// context are dropped as text; the files a reply's calls touched stay as edges
// to the file's absolute path, which is the id `connect fs` gives the same
// file.
//
// A tool is a reader: it finds the session files under a root and assembles
// one file into a Session. Everything after that — scoping to a project, the
// graph, the cursor — is shared and lives in this file.
package sessconn

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kgatilin/muninn/stream"
)

type Options struct {
	// Tool is "claude" or "codex".
	Tool string
	// Root is the tool's session tree; empty means the tool's default.
	Root string
	// Projects scopes the run; see Match. Empty takes every session.
	Projects []string
	// Cursor is the value of the previous run's cursor: the newest file
	// modification time it processed, RFC 3339 with nanoseconds.
	Cursor string
}

// Session is one session file, assembled.
type Session struct {
	ID      string
	CWD     string
	Branch  string
	Started time.Time
	Turns   []*Turn
}

// Turn is a user's message and what the agent said until the next one. One
// message may be followed by hours of work, so a turn is not a node: the
// message is one, and every reply another.
type Turn struct {
	At      time.Time
	User    string
	Replies []*Reply
	// Calls made before the agent said anything belong to the message.
	Calls
}

type Reply struct {
	At   time.Time
	Text string
	// Calls are those the agent made after saying this and before saying more.
	Calls
}

// Calls are what tool calls left behind: the files read and edited, and the
// rules a muninn command printed for the agent.
type Calls struct {
	Reads, Edits, Shown []string
}

// shortReply is the runes under which a reply is not a node of its own: "Let
// me read the file." says nothing a search should land on. It goes with the
// reply after it, or with the one before when it is the turn's last.
const shortReply = 200

// replies are the turn's replies as they are written, the short ones folded.
func (t *Turn) replies() []*Reply {
	var out []*Reply
	var held *Reply
	for _, r := range t.Replies {
		if held != nil {
			r = join(held, r)
			held = nil
		}
		if utf8.RuneCountInString(r.Text) < shortReply {
			held = r
			continue
		}
		out = append(out, r)
	}
	switch {
	case held == nil:
	case len(out) == 0:
		out = append(out, held)
	default:
		out[len(out)-1] = join(out[len(out)-1], held)
	}
	return out
}

func join(a, b *Reply) *Reply {
	out := &Reply{At: a.At, Text: a.Text + "\n\n" + b.Text, Calls: a.Calls}
	for _, path := range b.Reads {
		out.Reads = addPath(out.Reads, "", path)
	}
	for _, path := range b.Edits {
		out.Edits = addPath(out.Edits, "", path)
	}
	for _, id := range b.Shown {
		if !slices.Contains(out.Shown, id) {
			out.Shown = append(out.Shown, id)
		}
	}
	return out
}

// reader is one tool's half.
type reader struct {
	// tool is the value of the `tool` attr and the middle of every id.
	tool        string
	defaultRoot string
	// session names the session a file holds, or says the file is not one.
	session func(root, path string) (id string, ok bool)
	// read assembles the file. It asks keep once, as soon as the file has
	// stated the session's directory, and stops reading on a no: most of a
	// scoped run's files belong to other projects, and a session file is
	// megabytes. A nil session is a file that was not kept or has nothing to
	// say, such as a subagent's.
	read func(path, id string, keep func(cwd string) bool) (*Session, error)
}

var readers = map[string]reader{
	"claude": claudeReader,
	"codex":  codexReader,
}

// Tools lists the connectors this package has, sorted.
func Tools() []string {
	var names []string
	for name := range readers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultRoot is where a tool keeps its sessions.
func DefaultRoot(tool string) string { return readers[tool].defaultRoot }

// Run reads every session file modified after the cursor and writes each
// session whole. Nodes are idempotent by id and the engine skips unchanged
// hashes, so re-emitting a session that grew costs only its new turns. There
// is no sweep: sessions are not deleted.
func Run(w *stream.Writer, o Options) error {
	r, ok := readers[o.Tool]
	if !ok {
		return fmt.Errorf("no session reader for %q", o.Tool)
	}
	root := o.Root
	if root == "" {
		root = r.defaultRoot
	}
	root, err := filepath.Abs(expandHome(root))
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("root %s is not a directory", root)
	}
	var since time.Time
	if o.Cursor != "" {
		if since, err = time.Parse(time.RFC3339Nano, o.Cursor); err != nil {
			return fmt.Errorf("cursor %q: %w", o.Cursor, err)
		}
	}
	match, err := NewMatch(o.Projects)
	if err != nil {
		return err
	}

	newest := since
	e := emitter{w: w, tool: r.tool, seen: map[string]bool{}}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is not a reason to stop.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		id, ok := r.session(root, path)
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mtime := info.ModTime()
		if !mtime.After(since) {
			return nil
		}
		if mtime.After(newest) {
			newest = mtime
		}
		s, err := r.read(path, id, func(cwd string) bool { return match.Session(cwd, ProjectRoot(cwd)) })
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", path, err)
			return nil
		}
		// A session that never states its directory belongs to no project.
		if s == nil || s.CWD == "" || len(s.Turns) == 0 {
			return nil
		}
		return e.session(s, ProjectRoot(s.CWD))
	})
	if err != nil {
		return err
	}
	if !newest.IsZero() {
		return w.Cursor(newest.UTC().Format(time.RFC3339Nano))
	}
	return w.Flush()
}

// emitter writes sessions as the graph the design names: message and reply in
// session, reply answers message, each next the one before it, session on
// branch, branch of repo.
type emitter struct {
	w    *stream.Writer
	tool string
	// seen keeps a repo or branch shared by many sessions to one node a run.
	seen map[string]bool
}

func (e *emitter) session(s *Session, project string) error {
	repo := filepath.Base(project)
	sessionID := "session:" + e.tool + ":" + s.ID
	repoID := "repo:" + project

	attrs := map[string]string{"tool": e.tool, "repo": repo, "cwd": s.CWD}
	if s.Branch != "" {
		attrs["branch"] = s.Branch
	}
	if !s.Started.IsZero() {
		attrs["started"] = s.Started.UTC().Format(time.RFC3339)
	}
	if err := e.w.Node(stream.Node{ID: sessionID, Kind: "session", Attrs: attrs}); err != nil {
		return err
	}
	if err := e.structural(stream.Node{ID: repoID, Kind: "repo", Attrs: map[string]string{"name": repo}}); err != nil {
		return err
	}
	if s.Branch == "" {
		if err := e.w.Edge(stream.Edge{From: sessionID, To: repoID, Kind: "of"}); err != nil {
			return err
		}
	} else {
		branchID := "branch:" + project + "@" + s.Branch
		if !e.seen[branchID] {
			if err := e.structural(stream.Node{ID: branchID, Kind: "branch", Attrs: map[string]string{"repo": repo, "branch": s.Branch}}); err != nil {
				return err
			}
			if err := e.w.Edge(stream.Edge{From: branchID, To: repoID, Kind: "of"}); err != nil {
				return err
			}
		}
		if err := e.w.Edge(stream.Edge{From: sessionID, To: branchID, Kind: "on"}); err != nil {
			return err
		}
	}

	attrsOf := func() map[string]string {
		attrs := map[string]string{"tool": e.tool, "session": s.ID, "repo": repo, "cwd": s.CWD, "project": project}
		if s.Branch != "" {
			attrs["branch"] = s.Branch
		}
		return attrs
	}
	previous := ""
	// part writes one node of the conversation with its place in it and the
	// edges its calls left.
	part := func(id, kind, text string, at time.Time, calls Calls) error {
		node := stream.Node{ID: id, Kind: kind, Text: text, Attrs: attrsOf()}
		if !at.IsZero() {
			node.At = &at
		}
		if err := e.w.Node(node); err != nil {
			return err
		}
		if err := e.w.Edge(stream.Edge{From: id, To: sessionID, Kind: "in"}); err != nil {
			return err
		}
		if previous != "" {
			if err := e.w.Edge(stream.Edge{From: previous, To: id, Kind: "next"}); err != nil {
				return err
			}
		}
		previous = id
		for _, c := range []struct {
			kind    string
			targets []string
		}{{"reads", calls.Reads}, {"edits", calls.Edits}, {"injected", calls.Shown}} {
			for _, to := range c.targets {
				if err := e.w.Edge(stream.Edge{From: id, To: to, Kind: c.kind}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for n, t := range s.Turns {
		messageID := ""
		if t.User != "" {
			messageID = fmt.Sprintf("message:%s:%s:%d", e.tool, s.ID, n+1)
			if err := part(messageID, "message", "User: "+t.User, t.At, t.Calls); err != nil {
				return err
			}
		}
		for k, r := range t.replies() {
			id := fmt.Sprintf("reply:%s:%s:%d:%d", e.tool, s.ID, n+1, k+1)
			calls := r.Calls
			if messageID == "" && k == 0 {
				// A turn the agent opened has no message to carry its first calls.
				calls = join(&Reply{Calls: t.Calls}, &Reply{Calls: r.Calls}).Calls
			}
			if err := part(id, "reply", "Assistant: "+r.Text, r.At, calls); err != nil {
				return err
			}
			if messageID != "" {
				if err := e.w.Edge(stream.Edge{From: id, To: messageID, Kind: "answers"}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (e *emitter) structural(n stream.Node) error {
	if e.seen[n.ID] {
		return nil
	}
	e.seen[n.ID] = true
	return e.w.Node(n)
}

// assembly is how a reader builds a session: it says what it saw in file
// order, and the turn boundaries fall out. A turn's ordinal is its position
// here, and a session file only grows at its end, so the ordinal of a turn is
// the same on every read.
type assembly struct {
	s *Session
}

func (a *assembly) current() *Turn {
	if len(a.s.Turns) == 0 {
		return nil
	}
	return a.s.Turns[len(a.s.Turns)-1]
}

// user opens a turn.
func (a *assembly) user(at time.Time, text string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	// A message recorded twice in a row, with nothing said between, is one.
	if t := a.current(); t != nil && t.User == text && len(t.Replies) == 0 {
		return
	}
	a.s.Turns = append(a.s.Turns, &Turn{At: at, User: text})
}

// reply adds agent text to the open turn. Agent text before any user message
// — a session resumed from a summary, a scheduled run — opens a turn of its
// own, with no user half.
func (a *assembly) reply(at time.Time, text string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	t := a.current()
	if t == nil {
		t = &Turn{At: at}
		a.s.Turns = append(a.s.Turns, t)
	}
	t.Replies = append(t.Replies, &Reply{At: at, Text: text})
}

// calls is where a tool call is recorded: under the reply it followed, or
// under the message when the agent has said nothing yet.
func (a *assembly) calls() *Calls {
	t := a.current()
	switch {
	case t == nil:
		return nil
	case len(t.Replies) > 0:
		return &t.Replies[len(t.Replies)-1].Calls
	}
	return &t.Calls
}

func (a *assembly) reads(path string) {
	if c := a.calls(); c != nil {
		c.Reads = addPath(c.Reads, a.s.CWD, path)
	}
}

// ruleID is a rule's id as `muninn search` and `muninn rules` print it.
var ruleID = regexp.MustCompile(`\brule:[0-9a-f]{12}\b`)

// asksMuninn says whether a tool call runs muninn, and shown takes the rules
// out of what such a call printed: the agent had them in front of it from this
// turn on, which is what the rules stage reads a later correction against.
func asksMuninn(call []byte) bool { return bytes.Contains(call, []byte("muninn")) }

func (a *assembly) shown(output []byte) {
	c := a.calls()
	if c == nil {
		return
	}
	for _, id := range ruleID.FindAll(output, -1) {
		if !slices.Contains(c.Shown, string(id)) {
			c.Shown = append(c.Shown, string(id))
		}
	}
}

func (a *assembly) edits(path string) {
	if c := a.calls(); c != nil {
		c.Edits = addPath(c.Edits, a.s.CWD, path)
	}
}

// addPath resolves a path against the session's directory and keeps one of
// each per turn. A relative path with no directory to resolve it against is
// dropped: it would name no file.
func addPath(list []string, cwd, path string) []string {
	if path == "" {
		return list
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return list
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	for _, have := range list {
		if have == path {
			return list
		}
	}
	return append(list, path)
}

// lines calls fn with every line of a file until it returns false. A session
// record can be megabytes — a tool result carries a whole file — so there is
// no line limit.
func lines(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 1 && !fn(line) {
			return nil
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func homePath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}
