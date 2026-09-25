# muninn

Local memory for coding agents and for your own notes. muninn indexes text that
lives elsewhere (markdown folders, source code, Claude Code and Codex sessions,
anything a connector can stream) as one graph, and searches it: BM25 plus
embeddings, fused, then personalized PageRank over the graph's edges.

One Go binary, no server to run. Search is a CLI call that prints for an
agent's context window, so an agent needs no MCP setup to use it: one line in
its instructions is enough.

```
$ muninn search app "why does the importer retry on 409"
12 hits under /src/app/
# guide
1 docs/importer.md:40-58 — Importer › Conflicts · 2026-08-14
  …a 409 means the batch landed on another worker; the importer retries once with a fresh lease…
# code
2 internal/importer/lease.go:12-47 — package importer › func (l *Lease) Renew(ctx context.Context) error · 2026-09-02
  …
# talk
~3 reply:claude-code:4f1c9e2a:7:1 · 2026-09-03
  …we keep the single retry: a second one hid the lease bug for a week…
# rule
4 rule:9a0e51c2b7d4:1 · 2026-09-03 · 3 sources
  type: process
  Retry an import batch at most once on 409 and log the lease id.
```

`~` marks a hit the graph walk reached rather than the text match. Here the
conversation turn that changed `lease.go` comes up through its `edits` edge.

The name is Odin's raven of memory.

## Install

```
go install github.com/kgatilin/muninn/cmd/muninn@latest
```

or from a clone: `make build` (to `bin/muninn`), `make test`. Go 1.25 or newer.

A new bank embeds with [Ollama](https://ollama.com) and
`qwen3-embedding:0.6b` by default, locally and at no cost:

```
ollama pull qwen3-embedding:0.6b
```

## Quick start: a folder of notes

```
muninn bank add notes
muninn connector add notes docs --preset fs -- --root ~/notes
muninn index notes
muninn search notes "how do agents keep memory between sessions"
```

`index` is incremental. Run it again after the notes change and it embeds only
the chunks whose text changed.

## How it works

- **Bank**: a named, isolated index with its own connectors, embedder and
  settings, under `~/.muninn/banks/<name>`. One per project or concern.
- **Connector**: any command that writes the *graph stream* to stdout, as JSON
  lines of nodes, edges, deletes, a cursor and a sweep. Three ship with
  muninn: `fs` (files and directories), `claude` and `codex` (agent sessions).
  A connector of your own has the same standing as these.
- **Engine**: chunks each node's text (`text` for markdown, `go` along
  declarations, `yaml` along keys, or `none`), embeds the chunks, keeps a BM25
  index beside the vectors, and stores the edges.
- **Search**: dense and BM25 rankings fused by reciprocal rank, and the best
  hits seed personalized PageRank over the edges. A result list has text hits
  and the structural nodes around them: sessions, branches, tickets, entities.
  The hits are grouped by class (guide, code, talk, rule…), so thousands of
  conversation turns cannot crowd out a project's one design document.

Three optional stages run inside `index` and use an LLM:

- **Semantic layer**: entities that passages mention, written through a gate
  that measures the graph and rejects writes that would turn it into a
  hairball.
- **Enrichments**: rules and decisions drawn from conversations and design
  documents. Each one is a node tied to the passages that state it. A later
  passage that says otherwise rewrites the rule.
- **Compaction**: old conversation turns are removed after every stage has
  read them. What was drawn from them stays.

`docs/design.md` is the full design, including what each part does not do.

## Memory for a coding project

One bank over a project's agent sessions and its code:

```
muninn bank add app
muninn connector add app claude --preset claude -- --project ~/src/app
muninn connector add app codex  --preset codex  -- --project ~/src/app
muninn connector add app code   --preset fs -- --root ~/src/app \
    --glob '**/*.go' --glob '**/*.md' --glob '**/*.yaml' \
    --chunker-for '**/*.go=go' --chunker-for '**/*.yaml=yaml'
muninn index app
```

`--project` takes a path or a bare repo name and covers the repo's worktrees.
Each user message and each agent reply is its own node. A reply has `reads`
and `edits` edges to the absolute paths of the files it touched, and `fs` uses
the same ids. That is how a conversation and the code it changed become one
graph.

Then tell the agent about it, in `CLAUDE.md` or `AGENTS.md`:

```
This project is indexed in the muninn bank `app`. Before grep, ask it:
  muninn search app "<question>"
  muninn rules app "<what you are about to do>"
```

### Rules drawn from the graph

```
muninn bank set app enrich.model=vertex:gemini-3.8-flash
muninn bank set app enrich.process.preset=process            # what the user told the agents
muninn bank set app enrich.architecture.preset=architecture  # what the design documents require
muninn bank set app enrich.architecture.paths='**/docs/**/*.md'
muninn bank set app enrich.decisions.preset=decisions        # what the system is meant to be, for a project designed in talk
muninn index app

muninn rules app                                             # everything drawn, newest first
muninn rules app "add a storage backend"                     # what bears on the work at hand
```

An enrichment selects nodes by kind or path, reads each one with its nearest
neighbours, and keeps what the model draws. A custom one is
`enrich.<name>.prompt=@file` with `kinds` or `paths`. The session connectors
record which rules a session was shown, and a correction from the user is read
against those first.

### Entities

```
muninn bank set app semantic.model=gemini
muninn bank set app semantic.method=llm
muninn bank set app semantic.kinds=message,reply,rule   # the model reads the talk, not the code
```

### Keys and compaction

```
muninn bank set app 'keys.ticket=[A-Z]+-\d+'    # a design folder and a branch named after one ticket meet at a ticket node
muninn bank set app compact.kinds=message,reply
muninn bank set app compact.after=60d           # older sessions keep their session node and what was drawn from them
```

### Classes

A node's class is its kind unless the bank says otherwise. The classes are
answered in the order they were set, each ranked on its own:

```
muninn bank set app class.guide.paths='**/README.md,**/docs/**'
muninn bank set app class.code.paths='**/*.go'
muninn bank set app class.talk.kinds=message,reply
```

## Looking around

```
muninn search app "…"                        # --full, --json, --no-graph, --kind, --filter k=v, --since
muninn search app --from 'branch:/src/app@feature-x'   # no query: what surrounds a node
muninn node app <id> --neighbors
muninn node app <id>:42-67                   # the chunks holding those lines, as a hit's locator names them
muninn bank show app
muninn graph stats app                       # kinds, hubs, components, degree exponent, fractal dimension
muninn semantic log app --rejected           # what the gate turned down, and why
muninn export app --graphml -o app.graphml   # for Gephi
muninn up                                    # keep banks loaded; the page on http://127.0.0.1:7811
```

`muninn up` keeps every bank in memory, so `search` answers without reading the
index from disk, and serves a read-only page with the graph drawn and search
hits highlighted on it. With `bank set app index.every=15m` it also re-indexes
the bank on that period.

## Models and keys

Every setting goes through `muninn bank set <bank> key=value`, and
`muninn bank set --help` lists them. `bank.yaml` is never edited by hand.

| role | providers |
|------|-----------|
| embedder (`embedder=`) | `ollama` (default), `openai`, `vertex`, `noop` |
| chat (`semantic.model=`, `enrich.model=`) | `ollama`, `anthropic`, `gemini`, `vertex` |

API keys are read from the environment when a provider is first used and are
never written to disk: `OPENAI_API_KEY`, `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`.
`vertex` takes the project from `GOOGLE_CLOUD_PROJECT` or the quota project of
the application default credentials, the location from `GOOGLE_CLOUD_LOCATION`
(`global` by default), and the token from
`gcloud auth application-default login`.

## Cost

Every call to a hosted model (an embedding batch, a query embedding, a chat
completion) is appended as a line to `~/.muninn/usage.jsonl`, with the bank,
the purpose, the model and the token counts the provider reported. Calls to
local models are not recorded.

```
muninn usage [bank] [--by bank|model|purpose|day] [--since 2026-01-02]
muninn usage price <provider:model> <$ per 1M in> [<$ per 1M out>]   # override the built-in price table
```

The totals are estimates: failed requests and cached-token or batch discounts
are not counted.

## Writing a connector

A connector is any process that writes JSON lines to stdout:

```json
{"node": {"id": "doc:notes/japan.md", "kind": "document", "text": "...", "hash": "sha256:...", "attrs": {"path": "notes/japan.md"}}}
{"edge": {"from": "doc:notes/japan.md", "to": "dir:notes", "kind": "in"}}
{"delete": {"id": "doc:notes/old.md"}}
{"cursor": {"value": "seq:58025"}}
{"sweep": {}}
```

```
muninn connector add notes feed -- ./my-connector
```

A cursor is an opaque checkpoint. The engine stores it once everything before
it is committed and passes it back to the connector on the next run in
`MUNINN_CURSOR`, so a log reader can resume where it stopped. A sweep says the
run was complete: nodes this connector made earlier and did not emit this time
are deleted.

In Go, `github.com/kgatilin/muninn/stream` has the record types and a writer.
The "The graph stream" section of `docs/design.md` is the specification.

## Status

Pre-1.0. The stream format and the bank settings can still change between
versions. `muninn index --full` makes every connector start over.

## License

MIT, see `LICENSE`. The vendored d3 modules in `internal/ui/static/vendor` are
ISC.
