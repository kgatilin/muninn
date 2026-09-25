# muninn

**Project memory for coding agents, built from what is already on disk.**

A project's knowledge is spread over its code, its docs and hundreds of
Claude Code and Codex sessions. The sessions hold the most, and nothing reads
them again. That is where the user explained why the importer retries only
once, rejected a design, or corrected the agent for the third time about how
commit messages are written. The next session starts from zero. It greps the
code, finds *what* the code does and not *why*, and repeats the mistake the
last one was corrected for.

muninn indexes all of it (code, docs, notes and every agent conversation) into
one searchable graph on your machine. An agent asks it before it greps. It gets
back the design doc, the function, the conversation turn where the decision
was made, and the rules the user has already stated, in one answer.

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

Hit 3 did not match the query's words. The graph walk reached it: that reply
edited `lease.go`, and `lease.go` matched. `~` marks such hits.

## What it gives you

- **Agents stop repeating corrections.** muninn reads the conversations and
  pulls out the rules the user gave ("never edit `bank.yaml` by hand", "one
  commit per logical change") and the decisions about what the system is meant
  to do. Each rule links to the passages that state it and is rewritten when a
  later one says otherwise. `muninn rules app "<task>"` before a piece of work
  returns the ones that apply.
- **The "why" is next to the "what".** Each agent reply is tied to the files it
  read and edited. A search that finds a function also brings up the discussion
  that changed it. A search that finds a discussion brings up the code and the
  design doc of the same ticket.
- **Nothing to remember to save.** Memory tools that agents write to hold only
  what an agent chose to write down. muninn reads the sources as they already
  are: session logs, the repo, a notes folder. It re-indexes them incrementally,
  on demand or every few minutes.
- **Answers sized for a context window.** Output is grouped by class (guides,
  code, tests, talk, rules), and each class is ranked on its own. A
  thousand chat turns cannot push the one design doc off the list. Each hit is
  a `path:lines` locator and a one-line snippet, and the agent opens only what
  it needs.
- **Local and cheap by default.** One Go binary, an index in
  `~/.muninn`, embeddings from a local Ollama model. Hosted models (OpenAI,
  Gemini, Vertex, Anthropic) are optional. Every paid call is logged, and
  `muninn usage` shows what it cost.
- **No server or MCP setup.** It is a CLI. One paragraph in `CLAUDE.md` or
  `AGENTS.md` is the whole integration, and it works the same for any agent
  that can run a shell command.
- **Any source.** A connector is any program that prints JSON lines of nodes and
  edges. Files, Claude Code and Codex come built in. A wiki export, a ticket
  tracker or a chat log is a script in whatever language is at hand.

## Where it fits

| | finds | misses |
|---|---|---|
| grep / code search | the exact text | why it is so, anything said in a conversation |
| a vector store over docs (RAG) | passages close to the question | how passages relate: which talk changed which file |
| agent-written memory files | what an agent decided to save | everything it did not |
| **muninn** | text, meaning and the links between code, docs and talk | only what no source holds |

Under the hood: BM25 and embeddings fused by reciprocal rank, then personalized
PageRank over the graph, so relevance flows along edges from the direct
matches. An optional LLM stage adds entities through a gate that measures the
graph and rejects writes that would tie everything to everything. The name is
Odin's raven of memory.

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
