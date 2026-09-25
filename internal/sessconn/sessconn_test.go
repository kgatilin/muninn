package sessconn

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kgatilin/muninn/stream"
)

// graph is a run's output, read back.
type graph struct {
	nodes  map[string]stream.Node
	edges  []string // "from -kind-> to"
	cursor string
}

func run(t *testing.T, o Options) graph {
	t.Helper()
	var buf bytes.Buffer
	if err := Run(stream.NewWriter(&buf), o); err != nil {
		t.Fatal(err)
	}
	g := graph{nodes: map[string]stream.Node{}}
	r := stream.NewReader(&buf)
	for {
		rec, err := r.Next()
		if err == io.EOF {
			return g
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case rec.Node != nil:
			g.nodes[rec.Node.ID] = *rec.Node
		case rec.Edge != nil:
			g.edges = append(g.edges, rec.Edge.From+" -"+rec.Edge.Kind+"-> "+rec.Edge.To)
		case rec.Cursor != nil:
			g.cursor = rec.Cursor.Value
		case rec.Sweep != nil:
			t.Fatal("a session connector must not sweep")
		}
	}
}

func (g graph) has(t *testing.T, edge string) {
	t.Helper()
	if !slices.Contains(g.edges, edge) {
		t.Errorf("no edge %q in\n  %s", edge, strings.Join(g.edges, "\n  "))
	}
}

// leaks are strings the fixtures put where a turn's text must not reach.
var leaks = []string{"SECRET", "TOOL OUTPUT", "SIDECHAIN", "WRITTEN CONTENT", "COMPACT SUMMARY", "API Error", "Caveat", "cleared", "system-reminder", "TODO list", "task-notification", "interrupted", "ENVIRONMENT", "BASE INSTRUCTIONS", "DIFF BODY", "GUARDIAN", "passwd", "SUBAGENT REPORT", "HOOK OUTPUT"}

func (g graph) noLeaks(t *testing.T) {
	t.Helper()
	for id, n := range g.nodes {
		for _, leak := range leaks {
			if strings.Contains(n.Text, leak) {
				t.Errorf("%s: text carries %q:\n%s", id, leak, n.Text)
			}
		}
	}
}

const claudeSession = "11111111-1111-1111-1111-111111111111"

func TestClaude(t *testing.T) {
	g := run(t, Options{Tool: "claude", Root: "testdata/claude", Projects: []string{"/repo/app"}})
	g.noLeaks(t)

	turn := func(n string) string { return "message:claude-code:" + claudeSession + ":" + n }
	reply := "reply:claude-code:" + claudeSession + ":1:1"
	want := map[string]string{
		turn("1"): "User: how does the parser work?",
		// Two short replies with a Read between them are one node.
		reply:     "Assistant: Let me read it.\n\nIt is a recursive descent parser.",
		turn("2"): "User: add error recovery",
		turn("3"): "User: /ticket-start APP-7",
		turn("4"): "User: also handle EOF", // typed while the agent worked: a queued_command
	}
	turns := 0
	for id, n := range g.nodes {
		if n.Kind != "message" && n.Kind != "reply" {
			continue
		}
		turns++
		if n.Text != want[id] {
			t.Errorf("%s text = %q, want %q", id, n.Text, want[id])
		}
	}
	if turns != len(want) {
		t.Errorf("%d messages and replies, want %d — the subagent file and the other project must not be read", turns, len(want))
	}

	first := g.nodes[turn("1")]
	if first.At == nil || !first.At.Equal(time.Date(2026, 1, 2, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("turn 1 at = %v, want the user message's time", first.At)
	}
	if first.Chunker != "" {
		t.Errorf("turn states chunker %q; the bank's default is meant to apply", first.Chunker)
	}
	for key, value := range map[string]string{"tool": "claude-code", "session": claudeSession, "repo": "app", "branch": "feature/x", "cwd": "/repo/app", "project": "/repo/app"} {
		if first.Attrs[key] != value {
			t.Errorf("turn attr %s = %q, want %q", key, first.Attrs[key], value)
		}
	}
	session := "session:claude-code:" + claudeSession
	if got := g.nodes[session].Attrs["started"]; got != "2026-01-02T10:00:00Z" {
		t.Errorf("session started = %q", got)
	}
	if got := g.nodes["repo:/repo/app"].Attrs["name"]; got != "app" {
		t.Errorf("repo name = %q", got)
	}

	g.has(t, turn("1")+" -in-> "+session)
	g.has(t, reply+" -in-> "+session)
	g.has(t, reply+" -answers-> "+turn("1"))
	g.has(t, turn("1")+" -next-> "+reply)
	g.has(t, reply+" -next-> "+turn("2"))
	g.has(t, turn("2")+" -next-> "+turn("3"))
	g.has(t, session+" -on-> branch:/repo/app@feature/x")
	g.has(t, "branch:/repo/app@feature/x -of-> repo:/repo/app")
	g.has(t, reply+" -reads-> /repo/app/parser.go")     // the call followed the reply
	g.has(t, turn("2")+" -edits-> /repo/app/parser.go") // relative, resolved against the cwd
	g.has(t, turn("2")+" -edits-> /repo/app/recover.go")
	g.has(t, turn("2")+" -edits-> /repo/app/nb.ipynb")
	reads := 0
	for _, e := range g.edges {
		if strings.Contains(e, "-reads->") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("%d reads edges, want 1: two reads of one file in a turn are one edge", reads)
	}
}

// A session with a detached HEAD has no branch, and hangs off the repo.
func TestNoBranch(t *testing.T) {
	g := run(t, Options{Tool: "claude", Root: "testdata/claude", Projects: []string{"other"}})
	session := "session:claude-code:22222222-2222-2222-2222-222222222222"
	g.has(t, session+" -of-> repo:/elsewhere/other")
	if _, ok := g.nodes[session].Attrs["branch"]; ok {
		t.Error("HEAD was taken for a branch")
	}
	kinds := map[string]int{}
	for _, n := range g.nodes {
		kinds[n.Kind]++
	}
	if len(g.nodes) != kinds["message"]+kinds["reply"]+2 || kinds["session"] != 1 || kinds["repo"] != 1 {
		t.Errorf("nodes by kind = %v, want a session, a repo and the conversation", kinds)
	}
}

func TestCodex(t *testing.T) {
	g := run(t, Options{Tool: "codex", Root: "testdata/codex"})
	g.noLeaks(t)

	old := "message:codex:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:"
	oldReply := "reply:codex:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1:1"
	item := "message:codex:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb:"
	itemReply := "reply:codex:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb:1:1"
	want := map[string]string{
		old + "1":  "User: fix the recovery",
		oldReply:   "Assistant: Looking at recover.go.\n\nFixed.",
		old + "2":  "User: thanks",
		item + "1": "User: rename the package",
		itemReply:  "Assistant: Renamed.",
	}
	turns := 0
	for id, n := range g.nodes {
		if n.Kind != "message" && n.Kind != "reply" {
			continue
		}
		turns++
		if n.Text != want[id] {
			t.Errorf("%s text = %q, want %q", id, n.Text, want[id])
		}
	}
	if turns != len(want) {
		t.Errorf("%d messages and replies, want %d — response_item and the guardian session must not be read", turns, len(want))
	}

	// The worktree is filed under its repo, on the branch the header states.
	worktree := "/repo/app/.worktrees/APP-7-recover"
	first := g.nodes[old+"1"]
	for key, value := range map[string]string{"tool": "codex", "repo": "app", "project": "/repo/app", "branch": "APP-7/recover", "cwd": worktree} {
		if first.Attrs[key] != value {
			t.Errorf("turn attr %s = %q, want %q", key, first.Attrs[key], value)
		}
	}
	g.has(t, "session:codex:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa -on-> branch:/repo/app@APP-7/recover")
	g.has(t, "session:codex:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb -of-> repo:/repo/app")
	// The patches followed the agent's first words, which are folded with its
	// last: the edits are the reply's.
	g.has(t, oldReply+" -edits-> "+worktree+"/recover.go")
	g.has(t, oldReply+" -edits-> "+worktree+"/old.go")
	g.has(t, oldReply+" -edits-> "+worktree+"/new.go")
	g.has(t, oldReply+" -answers-> "+old+"1")
	g.has(t, item+"1 -edits-> /repo/app/pkg.go") // changed before the agent said anything
	for _, e := range g.edges {
		if strings.Contains(e, "failed.go") {
			t.Error("a patch that failed became an edge")
		}
		if strings.Contains(e, "-reads->") {
			t.Errorf("codex emits no reads edges, got %q", e)
		}
	}
}

// The cursor is the newest mtime read; a run given it reads only newer files.
func TestCursor(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/codex")); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(root, "2026", "01", "02")
	older := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour + 5*time.Nanosecond)
	entries, _ := os.ReadDir(day)
	for _, e := range entries {
		mtime := older
		if strings.Contains(e.Name(), "bbbbbbbb") {
			mtime = newer
		}
		if err := os.Chtimes(filepath.Join(day, e.Name()), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	full := run(t, Options{Tool: "codex", Root: root})
	if full.cursor != newer.Format(time.RFC3339Nano) {
		t.Fatalf("cursor = %q, want %q", full.cursor, newer.Format(time.RFC3339Nano))
	}

	next := run(t, Options{Tool: "codex", Root: root, Cursor: older.Format(time.RFC3339Nano)})
	for id := range next.nodes {
		if strings.Contains(id, "aaaaaaaa") {
			t.Errorf("%s was re-read though its file is not newer than the cursor", id)
		}
	}
	if _, ok := next.nodes["message:codex:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb:1"]; !ok {
		t.Error("the file newer than the cursor was not read")
	}

	// Nothing new: the cursor stays where it was.
	idle := run(t, Options{Tool: "codex", Root: root, Cursor: full.cursor})
	if len(idle.nodes) != 0 || idle.cursor != full.cursor {
		t.Errorf("idle run: %d nodes, cursor %q", len(idle.nodes), idle.cursor)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		project, cwd, root string
		want               bool
	}{
		{"/dev/gd", "/dev/gd", "/dev/gd", true},
		{"/dev/gd", "/dev/gd/web", "/dev/gd", true},
		{"/dev/gd", "/dev/gd/.worktrees/x", "/dev/gd", true},
		{"/dev/gd", "/dev/gdx", "/dev/gdx", false},
		{"/dev/gd", "/wt/gd-feature", "/dev/gd", true}, // a worktree kept beside the repo
		{"gd", "/dev/gd/web", "/dev/gd", true},
		{"gd", "/wt/feature", "/dev/gd", true},
		{"gd", "/dev/gdx", "/dev/gdx", false},
		{"gd", "/dev/other", "/dev/other", false},
	}
	for _, c := range cases {
		m, err := NewMatch([]string{c.project})
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Session(c.cwd, c.root); got != c.want {
			t.Errorf("--project %s, cwd %s, repo %s: %v, want %v", c.project, c.cwd, c.root, got, c.want)
		}
	}
	if m, _ := NewMatch(nil); !m.Session("/anything", "/anything") {
		t.Error("no --project must take every session")
	}
}

func TestProjectRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	beside := filepath.Join(base, "repo-feature")
	for _, dir := range []string{filepath.Join(repo, ".git", "worktrees", "feature"), filepath.Join(repo, "web"), filepath.Join(beside, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitFile := "gitdir: " + filepath.Join(repo, ".git", "worktrees", "feature") + "\n"
	if err := os.WriteFile(filepath.Join(beside, ".git"), []byte(gitFile), 0o644); err != nil {
		t.Fatal(err)
	}
	for cwd, want := range map[string]string{
		repo:                                    repo,
		filepath.Join(repo, "web"):              repo,
		filepath.Join(beside, "sub"):            repo,
		"/gone/repo/.worktrees/APP-1":           "/gone/repo",
		"/gone/repo/.claude/worktrees/x/subdir": "/gone/repo",
	} {
		if got := ProjectRoot(cwd); got != want {
			t.Errorf("ProjectRoot(%s) = %s, want %s", cwd, got, want)
		}
	}
}

// A muninn call's output names the rules the agent was shown; they become
// `injected` edges of the turn the call ran in, and the output itself stays
// out of the turn's text. The same id in a call that did not run muninn is
// somebody's file, not a rule that was shown.
func TestShownRules(t *testing.T) {
	claude := filepath.Join(t.TempDir(), "claude")
	writeLines(t, filepath.Join(claude, "-repo-app", "33333333-3333-3333-3333-333333333333.jsonl"),
		`{"type":"user","timestamp":"2026-01-02T10:00:00Z","cwd":"/repo/app","message":{"content":"start on the parser"}}`,
		`{"type":"assistant","timestamp":"2026-01-02T10:00:01Z","message":{"content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"muninn rules app parser work"}},{"type":"tool_use","id":"c2","name":"Bash","input":{"command":"cat notes.txt"}}]}}`,
		`{"type":"user","timestamp":"2026-01-02T10:00:02Z","toolUseResult":{"stdout":"x"},"message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"1 rule:0123456789ab:1-1 · 2026-01-01\n  Keep the parser free of globals.\n2 rule:ba9876543210:1-1"}]}}`,
		`{"type":"user","timestamp":"2026-01-02T10:00:03Z","toolUseResult":{"stdout":"x"},"message":{"content":[{"type":"tool_result","tool_use_id":"c2","content":"rule:ffffffffffff"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-02T10:00:04Z","message":{"content":[{"type":"text","text":"Two rules apply."}]}}`,
	)
	g := run(t, Options{Tool: "claude", Root: claude})
	// The call came before the agent said anything: it is the message's.
	turn := "message:claude-code:33333333-3333-3333-3333-333333333333:1"
	if got, want := g.nodes["reply:claude-code:33333333-3333-3333-3333-333333333333:1:1"].Text, "Assistant: Two rules apply."; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	g.has(t, turn+" -injected-> rule:0123456789ab")
	g.has(t, turn+" -injected-> rule:ba9876543210")

	codex := filepath.Join(t.TempDir(), "codex")
	writeLines(t, filepath.Join(codex, "2026", "01", "02", "rollout-2026-01-02T10-00-00-dddddddd-dddd-dddd-dddd-dddddddddddd.jsonl"),
		`{"timestamp":"2026-01-02T10:00:00.000Z","type":"session_meta","payload":{"id":"dddddddd-dddd-dddd-dddd-dddddddddddd","cwd":"/repo/app","source":"cli","git":{"branch":"main"}}}`,
		`{"timestamp":"2026-01-02T10:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"start on the parser"}}`,
		`{"timestamp":"2026-01-02T10:00:02.000Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"k1","name":"exec","input":"muninn rules app parser"}}`,
		`{"timestamp":"2026-01-02T10:00:03.000Z","type":"response_item","payload":{"type":"function_call","call_id":"k2","name":"shell","arguments":"cat notes.txt"}}`,
		`{"timestamp":"2026-01-02T10:00:04.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"k2","output":"rule:ffffffffffff"}}`,
		`{"timestamp":"2026-01-02T10:00:05.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"k1","output":"1 rule:0123456789ab:1-1"}}`,
		`{"timestamp":"2026-01-02T10:00:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"One rule applies."}}`,
	)
	c := run(t, Options{Tool: "codex", Root: codex})
	c.has(t, "message:codex:dddddddd-dddd-dddd-dddd-dddddddddddd:1 -injected-> rule:0123456789ab")

	for _, e := range append(g.edges, c.edges...) {
		if strings.Contains(e, "rule:ffffffffffff") {
			t.Errorf("%q: an id in the output of a call that did not run muninn became an edge", e)
		}
	}
}

// A long piece of work is told in several replies, and each that says
// something is a node: the calls between two of them are the earlier one's. A
// reply of a few words goes with the one after it.
func TestRepliesAreNodes(t *testing.T) {
	long := func(word string) string { return strings.Repeat(word+" ", 60) }
	root := filepath.Join(t.TempDir(), "claude")
	say := func(at, text string) string {
		return `{"type":"assistant","timestamp":"2026-01-02T10:0` + at + `:00Z","message":{"content":[{"type":"text","text":"` + text + `"}]}}`
	}
	writeLines(t, filepath.Join(root, "-repo-app", "44444444-4444-4444-4444-444444444444.jsonl"),
		`{"type":"user","timestamp":"2026-01-02T10:00:00Z","cwd":"/repo/app","message":{"content":"rework the parser and go on until it is done"}}`,
		say("1", long("plan")),
		`{"type":"assistant","timestamp":"2026-01-02T10:01:30Z","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/repo/app/parser.go"}}]}}`,
		say("2", "Now the tests."),
		say("3", long("report")),
	)
	g := run(t, Options{Tool: "claude", Root: root})
	id := func(kind, n string) string { return kind + ":claude-code:44444444-4444-4444-4444-444444444444:" + n }
	if got := g.nodes[id("reply", "1:1")].Text; got != "Assistant: "+strings.TrimSpace(long("plan")) {
		t.Errorf("the first reply = %q", got)
	}
	if got := g.nodes[id("reply", "1:2")].Text; !strings.HasPrefix(got, "Assistant: Now the tests.\n\nreport") {
		t.Errorf("the short reply did not go with the one after it: %q", got)
	}
	if at := g.nodes[id("reply", "1:2")].At; at == nil || at.Minute() != 2 {
		t.Errorf("the second reply's time = %v", at)
	}
	if _, ok := g.nodes[id("reply", "1:3")]; ok {
		t.Error("the short reply is a node of its own")
	}
	g.has(t, id("reply", "1:1")+" -edits-> /repo/app/parser.go")
	g.has(t, id("reply", "1:1")+" -next-> "+id("reply", "1:2"))
	g.has(t, id("reply", "1:2")+" -answers-> "+id("message", "1"))
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
