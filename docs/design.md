# muninn — design

muninn indexes text that lives somewhere else and answers searches over it. A
source is described to it as a graph: nodes that may carry text, and edges
between them. It embeds the text, keeps a lexical index beside the vectors, and
ranks a query by hybrid retrieval followed by personalized PageRank over the
edges the source named.

It exists because the same engine had been written twice already — in
[wyrd](https://github.com/kgatilin/wyrd) over a Go code graph, and in an
agent's note journal over notes and entities — and a third copy was about to be
written over the coding-agent session log. The two
existing ones agree down to the constants: BM25 plus dense retrieval, fused,
the top hits seeding an ACL-push PPR with alpha 0.15 and epsilon 1e-4, vectors
keyed by content hash and embedder identity. What differs between them is the
domain, and the domain is what a connector is for.

## The model

Four things, and the engine knows nothing about any domain:

- **Bank** — a named, isolated index: its connectors, its embedder, its chunker
  settings, its directory on disk. `personal`, `work`, one per concern. Banks
  share no state.
- **Connector** — a process that writes the graph stream to stdout. Any command
  will do. The ones shipped with muninn are subcommands speaking the same
  protocol; there is no privileged built-in path.
- **Graph stream** — the only input contract. JSON lines, five record kinds.
- **Engine** — chunk, embed, index, search. One implementation for every bank.

## The graph stream

One JSON object per line.

```json
{"node": {"id": "doc:life/travel/japan.md", "kind": "document", "text": "...", "hash": "sha256:...", "attrs": {"path": "life/travel/japan.md"}, "chunker": "text"}}
{"node": {"id": "session:425eb8b5", "kind": "session", "attrs": {"tool": "claude-code", "repo": "app"}}}
{"edge": {"from": "obs:425eb8b5:17", "to": "session:425eb8b5", "kind": "in", "weight": 1.0}}
{"delete": {"id": "doc:life/old.md"}}
{"cursor": {"value": "seq:58025"}}
{"sweep": {}}
```

- **node** — `id` is the identity; a second node with the same id replaces the
  first. `text` is optional: a node with text is searchable, a node without it
  is structural (a session, a work item, a directory, an entity) and exists to
  carry edges. `hash` covers the text; an unchanged hash means no chunking and
  no embedding. `attrs` is a flat string map, returned with hits and usable as
  a filter. `chunker` optionally names the chunking strategy for this node.
  `at` optionally carries the node's time, for `--since` filtering.
- **edge** — `from`, `to`, `kind`, optional `weight`. An edge may name a node
  that has not arrived yet; it takes effect when the node does.
- **delete** — removes the node, the edges its own connector stated on it, and
  the semantic edges built on its text. An edge another connector stated stays
  and dangles: that connector still asserts it, and it holds again when the
  node comes back.
- **cursor** — an opaque checkpoint. The engine stores it once everything
  before it is committed, and hands it back to the connector on its next start
  in `MUNINN_CURSOR`. An incremental connector (a log reader) resumes from it.
- **sweep** — "this run was complete": every node this connector produced
  before and did not produce in this run is deleted. A full-scan connector (a
  directory walk) emits everything and ends with a sweep; unchanged hashes make
  that cheap, and the connector keeps no state of its own.

The engine records which connector produced each node. `delete` and `sweep`
reach only the emitting connector's own nodes.

A Go package, `muninn/stream`, holds the record types and a writer, so a
connector in Go is a loop and a few `w.Node(...)` / `w.Edge(...)` calls. A
connector in any other language writes the JSON by hand against this section.

## Chunking

A node's text may be any size — a whole markdown document is one node. The
engine splits it into chunks, embeds and lexically indexes the chunks, and
keeps them internal to the index: chunks are not graph nodes. A node's score is
the maximum over its chunks, and a hit returns the node together with the span
and snippet of its best chunk.

Chunkers are named strategies in an engine registry, each one an implementation
of `Chunk(text, params) -> []span`:

- `text` — headings and paragraphs packed up to a budget. Markdown and plain
  text.
- `go` — top-level declarations, each with its doc comment, packed up to the
  budget; a function over the budget is cut between the statements of its
  body. The prefix is the package clause, and the signature for a chunk that
  is one function or a piece of one, joined with ` › ` as a heading path is. A
  file that does not parse is chunked as `text`. After wyrd.
- `yaml` — top-level keys and list items, each with the comments above it,
  packed up to the budget; one over the budget is cut the same way one level
  down, and at line ends when it has no keys or items. Read from the lines, not
  parsed, so Helm templates chunk too; each `---` document on its own. The
  prefix is the document's `kind` and `metadata.name` when it has them, then
  the key path for a chunk that is one key or item, or a piece of one.
- `none` — the node is one chunk.

Selection: the node's `chunker` field, then the bank's default, then `text`.
The one parameter, `budget` (runes per chunk), is per chunker, in the bank
config. A chunker
name the engine does not know fails the run; falling back silently would
degrade search with nothing saying so.

A node's chunk list is keyed by (node hash, chunker name and version), so
changing the chunker re-chunks. Vectors are keyed by the chunk's own content
hash, so chunks that come out identical are not embedded again, and editing
one paragraph of a long document embeds one chunk.

**A custom chunker is a connector.** A source that knows its structure better
than any registered strategy emits its pieces as small nodes with
`chunker: none`, each with an edge to its document node. Two consequences: the
pieces are graph nodes, so they can carry their own edges and a hit addresses
the piece; and their ids, hashes and deletes are the connector's to keep
stable.

## Embedding

One embedder per bank. The interface is asymmetric — `Embed([]string)` for
documents, `EmbedQuery(string)` for queries — because the local models worth
using prompt the two differently. Adapters, lifted from wyrd: Ollama
(`qwen3-embedding:0.6b` by default, batched, concurrent), OpenAI, and a
deterministic no-op for tests. Bounded retries with backoff, from the journal.

`openai` is `text-embedding-3-small` by default over `/v1/embeddings`, in
batches of 100, retried on 429 and 5xx; `embedder.endpoint` points it at a
compatible server. Its key comes from `OPENAI_API_KEY` and is never stored. A
missing key is an error of the first embedding call and not of `bank set`, so
the bank can be made on a machine without one.

`vertex` is `gemini-embedding-001` over Vertex AI's `predict`, asked for 768
dimensions and normalized here, 50 texts to a request and four requests in
flight; documents and queries differ by `task_type`. A 429 and a 5xx are
retried, and a 400 halves the batch, since the API also limits the tokens of one
request. Project, location and token come from `internal/gcp`: the environment
(`GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `global` by default) and the
application default credentials of `gcloud auth application-default login`,
exchanged for a token over plain HTTP. A service-account key file is not read.

**Cost.** `Embed`, `EmbedQuery` and the chat client's `Complete` return, beside
their result, the tokens the provider reported for the call (`usage.Tokens`:
in, out, served requests); a local model returns none. The caller hands them to
`Bank.RecordUsage`, which appends one line to `usage.jsonl` under the muninn
home — one log for every bank, since the question asked of it is what muninn
costs. A line holds time, bank, purpose (`embed`, `embed_query`, `extract`,
`revise`, `merge`), `provider:model` and tokens, and no money: the cost is
worked out on reading, from the price table in `internal/usage` under the
overrides of `prices.yaml`, so a changed price reprices the history. Tokens of
batches served before a call failed are recorded; requests that failed are not,
nor are cached-token and batch discounts, and thinking counts as output. A line
that cannot be written is said on stderr and does not fail the work. Nothing
reconciles the figure with the provider's bill.

A vector's key is (chunk content hash, provider, model, dimensions,
normalization, recipe version). Changing the bank's embedder leaves the old
vectors in place under their own key and embeds everything under the new one.

## Search

1. The query is embedded and scored by cosine against every chunk vector; BM25
   scores it against every chunk.
2. The two chunk rankings are fused by reciprocal rank, `1/(60+rank)`, and a
   node takes the score of its best chunk. RRF is chosen
   over wyrd's calibrated softmax because its temperatures are tuned to one
   embedder and a bank's embedder is configurable.
3. Attribute filters apply (`--filter k=v`, `--since`).
4. The top fused hits seed personalized PageRank over the bank's edges:
   ACL residual push, alpha 0.15, epsilon 1e-4, hub damping, per-kind edge
   weights from the bank config (an unconfigured kind weighs 1, a 0 takes the
   kind out of the walk), edges taken as undirected. The push is muninn's own,
   after `archmotif/pkg/localpartition`: that package returns degree-normalized
   weights for its sweep cut, and the ranking here needs the mass itself.
   `--no-graph` skips the walk.
5. Results are nodes ranked by PPR mass, each with its fused text score, a flag
   saying whether it was a seed or was reached through the graph, its attrs,
   and the best chunk's span and snippet.
6. The ranking is cut into classes, each ranked on its own and taking at most
   `k` places. A node's class is its kind, unless the bank names classes of its
   own (`class.<name>.paths`, `class.<name>.kinds`): a node belongs to the
   first that takes it. A bank holds many more turns of talk than guides, and
   one ranking would answer with talk alone; a plan of a feature long built and
   the guide that holds now are different sources, and the answer says which is
   which. The bank's classes are answered first, in the order they were set,
   the rest by their best hit. A class is answered when one of its hits scored
   at least a third of the best fused score; within it a hit stays that did,
   or that holds at least a third of the class's best mass. Classes are read at
   query time from the bank's config: changing them costs no indexing. What
   this does not do: the thresholds are one constant, not tuned per bank, and
   a class of its own for each kind makes a long answer in a bank of many
   kinds.
7. Structural nodes reached by the walk are returned too — "this work item" is
   a legitimate answer. Nodes with no text take at most k/4 of the places, and
   at least one, and are listed after the classes: a hub collects mass from
   every seed under it and would otherwise sit above the document that matched.
8. An item of an enrichment carries the number of nodes that state it.
9. `--from <node id>` takes no text: the walk is personalized on that node
   alone, and the answer is what stands around it, by class. A branch answers
   with the design of its ticket, the turns of its sessions, the rules they
   stated. With no text score to go by, a text node stays that holds at least
   a twentieth of the most any holds; the rest came by way of a hub, the
   repository every branch is of.

## Banks are managed through the CLI

Everything about a bank lives in one folder, `~/.muninn/banks/<name>/`: its
definition in `bank.yaml`, its index beside it in `index/`. Removing the folder
removes the bank. `MUNINN_HOME` points somewhere other than `~/.muninn`.

`bank.yaml` is written by the CLI and never needs a hand edit. It stays YAML so
it can be read, diffed and copied to another machine, and the CLI is the only
writer:

```
muninn bank add <name> [--embedder <provider>:<model>] [--chunker <name>]
muninn bank ls
muninn bank show <name>
muninn bank set <name> <key>=<value>...
muninn bank rm <name>

muninn connector add <bank> <name> -- <command> [args...]
muninn connector ls <bank>
muninn connector rm <bank> <name>
```

- `bank add` with no flags makes a working bank: Ollama with
  `qwen3-embedding:0.6b`, the `text` chunker, default search settings. A bank's
  policy is its settings — embedder, chunker default and parameters, seed
  count, edge weights, the semantic layer's method and checks — and there is
  no separate policy object: flags on
  `bank add` set them at creation, `bank set` changes them afterwards
  (`bank set work chunkers.text.budget=1200 search.edge_weights.in=0.8`).
- Every write is validated before it lands: a chunker the registry does not
  have, an embedder spec that does not parse, a key that does not exist are
  refused with the list of what is accepted. `bank show` prints the settings
  with the index's size, per-connector node counts, cursors and last run.
- `bank set` on the embedder or a chunker says what it costs — a full re-embed
  or a re-chunk on the next `index` — and does it then, not immediately. The
  index keeps chunks and not node text, so a node is re-chunked when its
  connector emits it again: a full-scan connector does that on every run, a
  connector resuming from a cursor needs `index --full`, which forgets the
  cursor.
- `connector add` takes the command after `--`, verbatim.
  `muninn connector add personal notes -- muninn connect fs --root ~/life --glob '**/*.md'`.
- `connector rm` deletes the nodes that connector produced, which ownership
  makes possible; `bank rm` deletes the folder and asks first.

What the CLI writes:

```yaml
embedder:
  provider: ollama
  model: qwen3-embedding:0.6b
chunker: text
chunkers:
  text: {budget: 1600}
connectors:
  - name: notes
    command: [muninn, connect, fs, --root, ~/life, --glob, "**/*.md"]
  - name: sessions
    command: [/path/to/agentlog-connector]
search:
  seeds: 10
  edge_weights: {in: 1.0, contains: 0.3}
  degree_cap: {of: 200}
  mute: ["repo:scratch"]
semantic:
  method: llm
  model: gemini
  checks:
    fractal: {params: {high: 5}}
    singleton_share: {enabled: true}
```

The `semantic.*` keys: `method` (`off`, `llm`; `llm` needs a
`model` set first), `model` (`<provider>[:<model>]`), `endpoint` (the
provider's base URL, when it is not the default), `concurrency` (passages with
the model at once, 2), `mentions` (entities per text node, 5), `rounds`
(proposals per text node, 3), `hops` and `region_cap` (the measured region, 2 and 2000),
`checks.<kind>.enabled` and `checks.<kind>.<param>`. Only what differs from the
defaults is written; `bank show` prints every check with its effective
parameters, and the layer's size with how many text nodes are read and pending.

## The index on disk

Under the bank's `index/`, files only, single writer: the graph (nodes, edges,
producing connector, cursors), the chunk table, the BM25 index, and one vector
file per embedder key — a flat float32 matrix beside an id table, so a search
reads it without decoding. Everything there is derived and rebuildable by
re-running the connectors; vectors are the only part that costs anything to
lose, and their content-hash keys survive a rebuild.

`muninn index` takes a lock on the bank; `muninn search` reads the last
committed snapshot and needs no running process.

## CLI

Go, cobra, zsh completions.

Bank and connector management is above. The rest:

```
muninn index <bank> [--connector <name>] [--full] [--follow]
muninn search <bank> "<query>" [--filter k=v]... [--kind K] [--since <time>] [-k N]
                               [--chars N | --full] [--attrs a,b] [--json] [--no-graph]
muninn node <bank> <id> [--neighbors] [--json]
muninn nodes <bank> [--kind K] [--json]
muninn rules <bank> [query] [--type <enrichment>] [-k N] [--reset]
muninn semantic reset <bank>
muninn semantic log <bank> [--rejected] [-n N]
muninn graph stats <bank> [--json] [--top N]
muninn graph check <bank> [--json] [--top N]
muninn export <bank> --graphml [-o file] [--kind K]... [--edge-kind K]...
muninn connect fs --root <dir> --glob <pattern>... [--chunker <name>]
                  [--chunker-for '<glob>=<name>']... [--follow-symlinks]
```

`index` runs each connector once to completion. With `--follow` it keeps a
connector that does not exit attached and commits at every cursor. Nothing
needs a resident process: every command reads the committed snapshot or takes
the writer's lock for itself. `muninn up` is the optional one, below.

`bank show` carries the index's state; there is no separate `status`.

Agents reach it through the CLI, and the text output of `search` is written for
a context window: the directory every hit shares printed once, then per hit one
locator that can be opened (`id:first-last` line), the heading path it sits
under, and a one-line snippet placed over the query's terms; other chunks of
the same node that ranked are listed as line ranges. No scores, no blank lines.
`~` before the rank marks a hit that was not a seed and is there through the
graph; a node with no text is one line of id, kind and `k=v` attrs. The shared
directory is printed only when the ids are paths.

```
8 hits under /Users/me/garden/content/
1 notes/agents/memory.md:42-67 — Agent memory › Recall
  …the window of the chunk holding the query's terms…
  also :120-150
```

`--full` prints the whole chunk in place of the snippet, `--attrs` adds chosen
node attributes, `--json` gives the same fields with `score`, `mass` and `seed`
to a program.
When the embedder cannot be reached the ranking is lexical only, and stderr
says so.

`muninn up [--addr 127.0.0.1:7811] [--index-every 15m] [--open]` keeps every
bank loaded in one process — the graph, the lexical index and the vector matrix
— and loads a bank again when `state.gob` changes its mtime. `search` dials the
address first (`MUNINN_ADDR`, or the default) and sends the query with its
options and rendering; the answer's body is what the command would have printed.
With nothing listening, or something else on the port, the command loads the
bank itself as before, so the process is a cache and never a dependency. On one
bank of 7800 nodes and 33000 chunks of 3072 dimensions the load is 0.45 s and
the process holds about 750 MB for three banks; what remains of a query is the
hosted embedder's round trip for the query vector. A bank's
`index.every` is the period on which the process runs what `index` runs over
it, `--index-every` the period of the banks that set none; the process looks
once a minute for the banks whose period has passed since their last run, one
bank's failure logged and the rest still run. A run over a bank with nothing
new makes no paid call, so the period costs a walk of the sources. With
neither set the process writes nothing. `ui` is
the command's older name and still works.

The same process serves a local page over the same packages. A kind's nodes
are listed as a table over the drawing — the sessions with their dates,
branches and what each came to in messages, entities and rules — and a row
opens the drawing of that node's surroundings alone. While they are drawn the
left column lists them in place of the bank's summary — the session's rules,
topics and entities, each with its text — and stays as the nodes in it are
opened. Beside that: the banks and what `bank show` says about each, the bank's graph
drawn on a canvas with node and edge kinds that can be switched off, the
selected node's attrs, chunks and neighbours — for a node with no text, a branch
or a session, also what the nodes pointing at it are tied to: the entities,
rules and files of its messages, with how many of them are — and search with the hits lit on
the graph — seeds, text hits and nodes the walk reached told apart, sized by
the mass PPR gave them. It reads the committed snapshot and writes nothing. The
page draws the whole bank. With a limit set, a bank is drawn as that many nodes
of highest degree among the node kinds that are switched on, plus the hits and their neighbours: a kind switched off
takes none of the places, so a kind of leaves — rules, which hang off a few passages
each — is drawn once the kinds above it are off.

## Shipped connectors

All of them are `muninn connect <name>`, speak the graph stream, and have no
path into the engine a foreign connector lacks. `connector add` takes
`--preset <name> [flags]` as the short spelling of `-- muninn connect <name>
[flags]`; nothing else distinguishes a preset.

**`fs`** — files under a directory: documents, directories, links.
`--chunker-for '**/*.go=go'`, repeatable, states a chunker on the files its
glob matches, first match first; the rest get `--chunker`, or none and the
bank's default. The names are checked against the registry before the walk.

**`claude`, `codex`** — coding-agent sessions read from the tool's own files
(`--root`, defaulting to `~/.claude/projects` and `~/.codex/sessions`), so a
bank over them needs no event log and no other service.

- The text nodes are a **message**, what the user wrote, and a **reply**, one
  text the agent wrote, each its own node with its own time: one message may
  set off hours of work, and the replies are where that work is told. A reply
  under 200 runes — "let me read the file" — is not a node; it goes with the
  reply after it, or the one before when it is the last. The text opens with
  `User:` or `Assistant:`, so a passage says who speaks wherever it is read.
  Tool calls, tool results, reasoning and injected context are dropped.
- Structural nodes: the session, the branch `(repo, branch)`, the repo.
- Edges: message and reply `in` session, reply `answers` message, each `next`
  the one before it in the session, session `on` branch, branch `of` repo. A
  reply `reads` and `edits` the files of the calls that followed it, up to the
  next reply; calls made before the agent said anything are the message's.
  Attrs: tool, session, repo, branch, cwd, project.
- A bank indexed when a turn was one node keeps those nodes: sessions are not
  swept. `connector rm` and `connector add` again is how it is rebuilt.
- Incremental: the cursor is the newest modification time among the session
  files read. A run reads the files modified since and writes each of those
  sessions whole; ids are stable and unchanged hashes are skipped, so only the
  new messages and replies cost anything. No sweep — sessions are not deleted.
- `--project <path-or-name>`, repeatable, scopes a run to one project's
  sessions, so a bank can hold one project. A path takes a session whose
  directory is the path or under it; a name takes one whose directory has the
  name as a path segment. A worktree is filed under its main repo — by the
  `<repo>/.worktrees/` path, or by the `.git` file of one kept elsewhere — and
  matches through it.
- Codex is the harder of the two: two vocabularies, and two parallel accounts
  of a session of which one is read.

## Hybrid banks

A bank already takes any number of connectors, and a node already names its own
chunker, so sessions and code in one bank need no new mechanism:
`connect claude` beside `connect fs --glob '**/*.go' --glob '**/*.md'
--chunker-for '**/*.go=go'` over the project, each chunked its own way, one index, one search.

What joins them is an **id convention**: a file is identified by its absolute
path, by every connector. `fs` emits the file node under that id; the session
connectors, which drop tool calls as text, keep what the calls touched as
edges — reply `reads` file, reply `edits` file — to the same id. An edge may name
a node that has not arrived, so the order of connectors does not matter and a
file no `fs` connector covers is a dangling edge that costs nothing. With both
present, "which sessions touched this file" and "what code was this discussion
about" are one PPR walk, and a query that hits code pulls in the turns that
changed it.

A code connector finer than files — symbols with `calls` and `implements`
edges, wyrd's graph — is a connector that emits the file's id as the parent of
its symbols; the same convention carries it.

## Two kinds of edges, two curators

**Structural edges** are what a source knows for a fact: reply in session,
session on branch, file in directory, document links document. They arrive in
the stream, and their curator is the connector. The engine stores them as
given.

**Semantic edges** are what the text is about: entities, topics, and the
`mentions` edges that tie passages to them. No source states them; they are
built, and whatever builds them can build a hairball — a few entities named
"agent" and "config" attached to everything, and PPR mass spread evenly over a
graph that no longer says anything. So the semantic layer is built inside the
engine, and every write to it passes a gate. Curation is part of how the layer
is written, not a pass someone remembers to run afterwards.

This is the note journal's design for graph writes, taken whole.

### The gated write

The semantic stage runs inside `muninn index`, after the connectors have
committed and under the same lock, over the text nodes that are new or changed
— the index keeps, per text node, the hash it had and the extractor that read
it. A changed node loses its old mentions before it is read again. The stage
commits at the end, so the entities are chunked, embedded and indexed like any
node, and saves the graph every few hundred nodes, so an interrupted run keeps
what it did. For each text node it asks an extractor for a **patch** and
submits the patch to `propose`:

```
patch
  ├── determine the affected region, measure it
  ├── apply the operations, recording their inverses
  ├── measure the same region again; evaluate the enabled checks
  ├── accepted ──► the patch stays, and is recorded with its evidence
  └── rejected ──► undo in reverse order; return every violation to the extractor
```

Patch operations: add an entity, add a `mentions` edge, merge two entities,
split one, and **insert an intermediate node** — a topic between an entity and
the three hundred turns that mention it, a sub-entity for one of the three
things "agent" means. Extra nodes whose only job is to keep the structure are
a first-class operation, as in the journal.

A rejected patch goes back to the extractor with the evidence, and the
extractor proposes again — attach to a narrower entity, insert the topic, drop
the weakest mention — for a bounded number of rounds. The revision both
extractors fall back on reads the violation: over the entity budget the patch
loses the entities it mints, as many as the budget is over by and the weakest
first, and keeps its mentions of entities that exist; otherwise it loses its
weakest mention. What is still rejected
is dropped and recorded; the text node stays searchable by its words and
structural edges, and only lacks semantic ones. Every proposal, kept or not,
is a line of `index/semantic.log`: the text node, the operations, the region's
size, and per check the estimator version, parameters, both measurements, both
losses and the decision. `muninn semantic log` reads its tail.

The region is the patch's footprint grown by whole shells, `hops` times, up to
`region_cap` nodes; a shell that would pass the cap is cut in id order and the
evidence says so. It is grown over the union of the graph before the patch and
after it, so both measurements are of one domain and an edge the patch removed
cannot take its old neighbourhood out of the baseline. The baseline is read
through the applied patch — the graph as it is, minus what the patch changed —
so nothing is applied twice and nothing is copied. The whole graph is never
re-measured for one write, and the undo log is proportional to the patch.

The region is measured over the **semantic graph**: text nodes, entities,
topics, and the layer's own edges. Structural edges are left out. They are
facts their connectors own and no patch may rewrite them, so a gate that
measured them would hold a patch responsible for a shape it cannot change —
one directory over every document puts the whole bank within two hops, and no
region would have a dimension to lose. `graph stats` reports both graphs.

### The two metrics

Both are about complexity and both come from the journal's design; the
estimators are written here from their published definitions:

- **Scale-free degree structure** — the power-law exponent γ of the degree
  distribution, with the KS fit distance and the power-law/lognormal likelihood
  ratio R_ln beside it, because γ alone did not tell several graph families
  apart. Self-similarity of degree: hubs exist, and hubs of hubs, and no single
  node that is everything's neighbour.
- **Fractal-like structure** — the dimension estimate d_B by ball growth: how
  the number of nodes within r hops grows with r. A hairball reaches everything
  in two hops and has no dimension to speak of; a well-structured graph keeps
  growing at a steady rate.

The exponent is the discrete maximum-likelihood fit of Clauset, Shalizi and
Newman over the positive degrees of the region's nodes — their degrees in the
whole semantic graph, since the region's edge cuts through its boundary nodes'
neighbourhoods. k_min is the cutoff whose fit has the smallest KS distance;
the after-measurement reuses the baseline's cutoff, so a cutoff hopping
between two near-equal minima does not read as a change in γ. R_ln is the
log-likelihood ratio against a lognormal discretized by rounding and fitted on
the same tail, divided by its standard deviation (Vuong). It needs 50 nodes
with an edge and 10 in the tail. d_B is the least-squares slope of ln N(r)
against ln r, N(r) the mean ball size from 64 centres fixed by a hash of their
ids, over r = 1..6 while the mean ball is under nine tenths of the region; it
needs 50 nodes and three radii. It is a ball-growth approximation, not a
box-covering dimension: a ring reads 0.8 and a square lattice 1.6.

Starting targets, the journal's: γ in [2.0, 3.5], KS < 0.1, |R_ln| < 3, d_B in
[2.5, 4.5]. A target band is a reference, not an admission rule: a check
computes a distance from the band and accepts when the patch does not make it
worse — `loss_after - loss_before <= max_regression`. A region out of band
still accepts a patch that improves it or leaves it alone. A region too small
for an estimate reports `not_applicable` and does not block. The scale-free
loss is the distance of γ from its band, plus the excess of KS over 0.1, plus a
tenth of the excess of |R_ln| over 3; the weights are parameters.

One undefined estimate does block. A region that had a dimension and after the
patch saturates within three hops has not become too small to measure — it has
become the hairball — and the fractal check rejects that transition
(`reject_collapse`). Treated as `not_applicable`, the patch that attaches one
entity to everything would be the one patch the check never sees.

Checks are composable and configured per bank through `bank set`: kind,
version, parameters, enabled. Beside the two metrics, the journal's supporting
checks come along, over global live counters the patch operations keep and
undo restores: an entity budget (`max(0, E - (allowance + ratio*N))`, here
50 + 0.25 per text node: an entity is a hub over text nodes, so a layer holds
several times fewer of them — a text node is a document or a turn, not the
journal's short note) and the share of entities with one mention against a ceiling. The
second is off in a bank that says nothing: every entity has one mention when
it is first made, so on a young layer it refuses whatever would start it.

### The extractor

What proposes patches is a bank setting:

- `semantic.method=llm`, `semantic.model=<provider>[:<model>]` — a chat model
  names the entities, is shown existing candidates so it reuses before it
  mints, and on rejection reads the evidence and restructures. Providers:
  `ollama` for a local model (`/api/chat`, default `qwen3:4b`), `gemini`
  (`generateContent`, default `gemini-2.5-flash-lite`, key in `GEMINI_API_KEY`),
  `vertex` (the same call on Vertex AI, default `gemini-3.5-flash-lite`,
  credentials as the `vertex` embedder's) and `anthropic` (the Messages API, default `claude-haiku-4-5`, key in
  `ANTHROPIC_API_KEY`). Each is one plain HTTP call with the answer's JSON
  schema handed to the provider's own structured output, retried a few times
  on 429 and 5xx. A small local model is the weak option — canonicalization
  and restructuring are where it fails — so local is a choice, not the default
  assumption. A hosted provider's key is read from the environment of the
  `index` run; muninn stores no key anywhere, and a missing key stops the stage
  before it reads a node.

  The extractor works in two passes. The first names every
  pending text node, `semantic.concurrency` at a time, and mints nothing. The
  names are then counted over the run: names whose vectors are within cosine
  0.94 are one name — a spelling, a word order, an abbreviation beside its
  expansion; related names sit lower and stay apart — the one more nodes chose,
  and an entity of the layer takes a name folded onto it. An entity is minted
  only for a name `semantic.votes` text nodes chose (2 by default), and the
  entity budget is spent on the most chosen names first, so what the second
  pass proposes is already within it. The run logs how many names had 2, 3, …
  6 votes, which is what to pick `votes` from. A text node that arrives later
  reuses an entity by id from its candidates, which needs no second voice; a
  name it alone chose is not minted, and the votes of earlier runs are not
  kept. After the run the sweep takes any entity that fewer than `votes` text
  nodes mention, directly or under its topics: a rejected patch or a deleted
  document may leave one short. One bank of notes gave, without the votes,
  785 entities over 735 documents with 552 of them mentioned once; with
  `votes=3`, 230 entities of mean degree 6.7, γ 3.06 and d_B 2.63.

  The first pass's answers are kept in the state, under the node's hash and
  the extractor's id, and saved every 256 of them, until the second pass has
  read the node. A run stopped while naming is resumed by the next `index`
  from where it was, and asks only about the passages that have no answer;
  the votes are counted over all of them, kept and new.

  The pass over the layer then writes each entity a paragraph: the model reads
  the entity's name and up to eight excerpts of the text nodes that mention it,
  each from the chunk that first names it, and says what the thing is and what
  the notes do with it. The paragraph is a `describe` operation through the
  gate; it is kept in the node's `summary` and is part of the text the bank
  chunks, embeds and indexes under the entity, after its name and aliases. It
  is written for the entities the run minted or merged into and for any that
  has none, so a layer built before descriptions gets them on the next `index`.
  Names are compared — in the fold and in the merge pass — by the vector of the
  name alone, kept under the name's own hash.

  The model reads a budgeted excerpt: a node under 6000 runes whole, a longer
  one as its outline of heading prefixes and its leading text. Candidates come
  from the live layer and not from `search`, whose index on disk does not hold
  what the run has made so far: entities and topics whose name or alias occurs
  word for word in the node, and those whose name vector is closest to the
  node's leading chunks, 24 at most. An entity minted since the last commit has
  no vector yet and is found by its words alone. A name is normalized to the
  bank's tokens, one to four words. A simple English plural folds onto its
  singular when the bank holds the singular as a word — `worktrees` onto
  `worktree`, and neither `status` nor `class` — and the minted entity keeps
  the plural as an alias. The answer is validated against the graph before it
  is a patch: an id that is not in the layer, a name that does not normalize, a
  passage moved that does not mention the entity make an invalid proposal. In
  extraction the invalid entries are left out; in a revision the round falls
  back to dropping the weakest mention. An answer that is not JSON is asked for
  once more, then the node gets no patch. A failed request stops the stage, and
  the next `index` goes on from there: the extractor's id is
  `llm/<prompt version>/<provider>:<model>`, so a node is read once per model
  and prompt.

  On rejection the model sees the rejected operations, every violation with
  its measurements, and the three largest entities the patch touched, each
  with up to thirty of the passages that mention it and the topics under it.
  It answers one of: a revised mention list, `insert_intermediate`,
  `split_entity`, or giving up. Passages go to the model a few at a time
  (`semantic.concurrency`), each sent when the stage reaches one of the few
  before it, so its candidates miss at most what those few mint.

  After the text nodes are read and committed, a merge pass compares every
  entity the run minted with every other by the cosine of their name vectors;
  pairs at 0.92 and above go to the model ten at a time, and a confirmed pair
  is a `merge_entities` patch through the same gate. Pairs of two older
  entities are not asked about again, which also means a run interrupted
  before its merge pass leaves its pairs unjudged.

  Two names in different scripts — a name and its translation — sit lower than
  two spellings do, measured at 0.87–0.94 under a multilingual embedder. They
  are paired from 0.86, whichever run minted them, and each pair is asked
  about once (`State.Judged`). The merged entity is what takes a query in one
  language to what was written in the other: the lexical channel matches only
  the query's own language, and the walk crosses from there. What this does
  not do: two languages in one script are not told apart, and a wrong merge is
  undone only by resetting the layer.
- `semantic.method=off` — the default. A bank is useful without the layer.

`semantic.kinds=<kind>[,<kind>...]` names the node kinds the stage reads; empty, the default, is every kind with text. A hybrid bank
sets `semantic.kinds=message,reply`: phrases of source files are identifiers, and read
first they spend the entity budget before the conversation is reached. A kind left out
is not counted by the budget either. The pending nodes are read newest first
by their time, the undated after them, so a budget that binds is spent on
recent material.

An entity node carries text — its name and aliases — so the engine embeds it,
a query hits it through the ordinary channels, the hit seeds PPR, and mass
flows along `mentions` to every passage about it. That is HippoRAG's
query-entity seeding with no model call at query time.

The layer is the engine's own: no connector's `delete` or `sweep` reaches it,
a deleted text node takes its `mentions` with it, and
`muninn semantic reset <bank>` drops the whole layer to be built again —
which is also how two methods are compared on one bank.

### archmotif's part

The gate's metrics are regional and run on every write, so they live in the
engine, and the same estimators run over the whole bank for diagnosis.
`muninn graph stats <bank>` reports node and edge counts per kind, connected
components, degrees and hubs per node kind, and γ / KS / R_ln / d_B over the
full graph and over the semantic graph. `muninn graph check <bank>` sets those
against the bank's bands and names the nodes behind each miss: the highest
degrees for a scale-free miss, and for a fractal miss above the band the nodes
with the largest two-hop reach, estimated as the sum of their neighbours'
degrees. It diagnoses; it is how the targets and the check parameters get
chosen and how drift is noticed. It does not write.

archmotif is the wide-angle instrument for what the engine does not compute:
`export --graphml` hands it the graph, and clusters with their conductance,
coupling between clusters and the macro shape join `graph stats` from there.

The export is archmotif's GraphML dialect, which Gephi reads too: generated
XML ids with the node's own id in the `id` attribute, and on a node `label`
(title or name attr, a path's base name, the opening of its text, the id),
`kind`, `connector`, `chunks`, `degree`, `at`; on an edge `kind`, `weight`,
`connector`. archmotif refuses an edge whose end is not in the file, so an
edge to a node the bank does not hold is left out and counted on stderr.
`archmotif analyze`, `quotient --partition kind` and `graphml-scan` read it as
it is.

Structural hubs remain — a repo every session hangs off is a fact, and the
gate does not see it. Three search-time cuts cover that, all bank settings: a
per-kind edge weight, a per-kind degree cap above which a node's edges of that
kind are ignored, and a mute list of node ids. The stored graph stays what the
connectors said.

- `search.edge_weights.<kind>=<weight>` — 0 takes the kind out of the walk.
- `search.degree_cap.<kind>=<n>` — a node with more than n edges of the kind,
  counted in the stored graph and in both directions, has its edges of that
  kind left out of the walk; its other kinds still carry mass. 0 lifts the cap.
- `search.mute.add=<node id>`, `search.mute.remove=<node id>` — a muted node is
  not crossed by the walk and is never a seed or a hit, `--no-graph` included.
  The id is the value, so a path needs no escaping in the key.

`bank show` prints the caps on the search line and the muted ids under it.

### What this costs

The engine gains a model client and a write path with undo, which is the
largest thing in it after search. A model-built layer is the model's opinion
and two builds differ. First build over every turn of every session is hours
of inference locally, or a bill. Relations between entities (HippoRAG's
triples) are not extracted, only entity-to-passage `mentions`. The metric
targets were tuned on the journal's bipartite note-entity graph and on ~1000
node synthetic ones, so the bands are a starting point to be re-measured on a
bank. An extractor with no model, KeyBERT's method over the bank's term
statistics, was built first and taken out: its entities were phrases, and a
phrase is not always a thing. On a bank of sessions and code the hubs were
`testing`, `assert`, `client` and a verb an agent repeats, tied to hundreds of
nodes each. A cutoff searched per measurement and a sample of
centres make a regional estimate noisy at the scale of one small patch, and
with `max_regression` at zero some patches are rejected on noise; the
tolerance is the parameter for that.

## Enrichments

The structural graph is what the sources state. An enrichment is a rule of the
bank for drawing more out of it with a chat model: which nodes are read, what
the model is asked for, and what kind of node an answer becomes. The first use
is the rules a user gave the agents — not that provider, commit messages in one
line, never that tool — drawn from conversation turns, so the next session can
ask for them and a later correction changes what is kept; the second is the
rules of the architecture, drawn from a project's design documents. The stage
runs in `index` after the connectors and before the semantic stage, for a bank
with `enrich.model` and at least one enrichment that is whole.

An enrichment, `enrich.<name>.*`:

- `kinds`, `paths` — the selection: a text node of one of the kinds whose id
  matches one of the globs. Nodes of the semantic layer and of other
  enrichments are never selected.
- `prompt` — what an item is, as text or `@file`, copied into `bank.yaml`. The
  frame around it is the engine's: how the passages are shown, the form of the
  answer, that a later passage holds over an earlier one.
- `fields` — the strings an item carries beside its text; they become attrs of
  its node and work in `search --filter`. The answer's form is otherwise fixed:
  a list of items, each with `text` and `from`. A schema of the bank's own with
  a mapping onto the graph is not there.
- `kind` — the kind of the nodes made, `rule` by default.
- `neighbours`, `votes`, `ratio` — below; 4, 1 and 0.05.
- `preset` — a prompt shipped with muninn, with its kinds and votes:
  `process`, over messages and replies, and `architecture`, which selects nothing of its own:
  where a project keeps its design documents is the bank's to say.
  `decisions`, over messages and replies as well, is for a project designed
  in talk, with no tickets, branches or design folders: what the system does
  and must never do, what was ruled out, which trade-off was taken. `process`
  leaves those out on purpose — it keeps how the work is done — so the two
  read the same turns for different things, and the talk is paid for twice.

How it runs:

- **Neighbourhoods.** A node is read with the `neighbours` selected nodes
  nearest to it, by the mean of the vectors its chunks already have, so no
  embedding is paid for. The oldest node not read calls a neighbourhood, and
  every node in it counts as read: the calls are a fraction of the nodes. A
  node that arrives later is read with the older ones it resembles. A node is
  shown whole, up to 12000 runes kept from its head and its end, and a part of
  a conversation with the last 2000 runes of the part before it, over the
  `next` edge.
- **Items.** The model answers with items and, for each, the passages that
  support it. One supported by fewer than `votes` passages is dropped. Every
  supporting node gets a `states` edge to the item, so an item ties passages
  together as an entity does and is reached through the graph from any of them.
- **Settling.** Neighbourhoods are read ahead of the settling, twenty-four at
  once and at most ninety-six past the batch being settled — what a stopped
  run reads again. The items of sixteen neighbourhoods are
  settled in one call, against the kept items nearest to each of them by
  vector — twelve an item, sixty in all — the batches one after another in the
  order of time. One call a neighbourhood was what a run waited for: the
  calls cannot overlap, each deciding against what the one before it left. An
  item that repeats another of its batch is answered `same` with that item's
  number and lands on whatever the first became. An answer that cannot be used
  is asked for again a neighbourhood at a time.
  `same` adds the edges to the kept item,
  `update` also replaces its text with what holds now, `retire` removes it,
  `merge` puts kept items about one matter, with the new one, under the id of
  one of them, with a text that says what all of them said and the passages of
  all behind it, `new` mints one. The id does not change when the text does; the earlier
  wordings are not kept, the passages behind the edges are.
- **Speaking against.** An item that says several things — a broad one, made
  under the ratio's pressure — is rarely contradicted whole: a later statement
  overturns one part of it, and as a whole the two are not "about the same
  matter", so `update` does not reach it. Whatever its action, a new item may
  therefore name in `against` the items a part of which it overturns, each
  with its text as it stands without that part, or with none when nothing is
  left. Beside the enrichment's own items the settling shows the nearest items
  of the bank's other enrichments, which take no other action: a statement in
  a conversation can so correct a rule drawn from the design documents. The
  corrected item stays its enrichment's, and the node that spoke against it is
  one more of the nodes behind it, so it is as late as the correction.
  Consolidation looks for the same among the items already kept, each shown
  with its date: the later of two that contradict wins. What this does not do:
  the later statement wins whatever stands behind the earlier one, a turn of
  talk against two documents; and a statement that made no item of its own —
  "that part is legacy", said in passing — speaks against nothing.
- **Consolidation.** The same reading is turned on the items themselves: a
  kept item that has not been looked at as it stands is shown with the
  `neighbours` kept items nearest to it, and the model names those that are
  about one matter and the one item that says what they all say. The item that
  stays takes that text and the passages of the ones that go. It runs at the
  end of a run.
- **Ratio.** `ratio` items per selected node, and never fewer than ten, is
  what an enrichment should keep. It is a pressure to merge and never a reason
  to drop: an item no kept one can carry is always kept. Over the ratio the
  settling is told so and asked to let kept items carry what is new; a quarter
  over it — or over what the last pressed pass left, when that could not reach
  the ratio — and at the end of a run over it at all, the items are *pressed*: a
  consolidation pass over every item, each shown once, that merges what comes
  under one principle into one broader item of up to six sentences, until the
  count is back at the ratio or the pass is through. An item past 900 runes is
  left out of a pressed pass, so an enrichment whose matter is wider than its
  ratio ends over it, with a line in the log, and not in a few items that say
  everything. On one bank of a thousand design documents a hard cap had kept
  fifty items from the first sixth of the documents and dropped rules that were
  plainly their own from then on; that is why there is none.
- **The node.** `<kind>:<12 hex>`, owned by `~enrich:<name>`, `type` the
  enrichment's name, `at` the time of the latest supporting node, `repo`,
  `project` and `branch` from it. It is text like any connector's: the semantic
  stage reads it when `semantic.kinds` lets it.
- **What the agent was shown.** A session connector reads the output of a tool
  call that ran `muninn` and states an `injected` edge from the message or reply to every rule id
  in it. Kept items a neighbourhood's sessions were shown are listed first and
  marked: a correction is most likely about what the agent had before it.
- **Saved as it goes.** Sixteen neighbourhoods are read, eight at a time, then
  settled, and the state is saved. What the record holds for a node is a hash
  of what was shown of it, the prompt and the model, so a change of any reads
  it again and nothing else does.
- **Asking.** `muninn rules <bank> [--type <name>] "<what is about to be
  done>"` searches what the enrichments made; without a query it lists it,
  newest first. Nothing pushes items into a session: the agent's own
  instructions tell it to ask before a piece of work.

The stage does not pass the semantic gate. The gate measures the shape of a
layer built from every text node; items are few by the ratio, and under the
layer's votes and sweep a rule said once would not live.

What this leaves out:

- Nobody confirms an item. Whether a sentence is a standing rule or a request
  of the moment is the model's reading, and one read wrongly is served until a
  later passage says otherwise. `muninn rules --reset` draws them again;
  removing one by hand is not there.
- The model decides the quality. On one bank of 245 turns, read a turn at a
  time, a lite model kept 13 rules, three of them feature requests, and missed
  two the user had put plainly; a mid-size model kept 12 with no feature
  request among them. `enrich.model` is apart from `semantic.model` for this.
  Read in neighbourhoods the same bank took 96 calls for 245 turns, with 17
  repeats landing on kept rules; the calls are fewer and each is several whole
  passages, which came to three times the tokens.
- Nearness by vector is nearness of subject. A short correction sits in a turn
  whose text is mostly the agent's, and its neighbours are turns about that
  subject; which of them state one rule is the model's reading.
- An item is tied to the nodes it was drawn from and to nothing else: an
  enrichment states no edge between two nodes that exist, such as a file and
  the decision it implements.
- A document's time is when git says the file was added, the file's own time
  outside a work tree: a checkout dates every file by the clone. A design
  amended later stays as old as its first commit, and a file that was moved is
  as old as the move.
- `rules` ranks by age too: among the items a query matches, score and mass
  halve for every `--half-life` days (90) since the latest passage behind the
  item. A design nothing later contradicts is never retired, only outranked:
  what is old and still holds sinks with what is old and does not.
- Items are the bank's. A rule that holds in every project is drawn in each
  bank whose passages state it.

## Keys

Nodes of different connectors often carry the same name in their ids: a design
folder `docs/design/APP-12-cache/` and the branch `APP-12-cache-layer` the
design was built on. A bank names the pattern:

```
muninn bank set <bank> 'keys.ticket=[A-Z]+-\d+'
```

The keys stage goes first among the stages of `index`. It reads node ids and
nothing else, asks no model, and is worked out whole at every run. Every
distinct match that two or more nodes hold becomes a node `<name>:<match>` of
kind `<name>`, its text the match, owned by `~key:<name>`; each holder gets a
`has` edge to it. A pattern removed takes its nodes at the next run.

Through a ticket a rule drawn from a design document reaches the code: rule ←
`states` — document → `has` → ticket ← `has` — branch ← `on` — session →
`edits` → file. Nothing of git is read: the files of a branch are the files the
sessions on it touched.

What this leaves out:

- Files changed on a branch outside an agent's session are not tied to it; a
  connector over `git diff` would state that.
- The files are those touched while the design was built, which for an old
  rule may not be the files it is about.
- Only ids are matched: a ticket named in a message's text ties nothing.

## Compaction

Old conversations are read once and then weigh on every search. A bank says
which kinds go and after how long:

```
muninn bank set <bank> compact.kinds=message,reply
muninn bank set <bank> compact.after=60d
```

Compaction is the last stage of `index`, after the enrichments and the
semantic stage. A node of these kinds is `in` a container — a message in its
session. A container is compacted whole, when every such node in it is older
than `compact.after` and every stage that selects the node has a record of
reading it; a session with one unread or recent reply stays as it is.

The members leave the graph. Every edge they had to something outside the
container — `states` to an item, `mentions` to an entity, `edits` to a file,
`injected` from a rule — is the container's from then on, edges that fall
together summing their weights; edges among the members and to the container
go. So the session stays findable through the rules drawn from it, the
entities it was about and the files it touched.

The ids are kept in `State.Compacted` with the connector that emitted them,
and the engine does not hear that connector emit them, or an edge of theirs,
again. `connector rm` forgets them, so removing and adding a connector brings
the conversations back to be read anew.

What this leaves out:

- A compacted conversation cannot be read again: a changed prompt, model or
  frame version re-reads only what is still in the graph.
- "Read" is that a record exists, not that it is current. A node whose stage
  was stopped by a budget after a settings change goes with its old reading.
- A session's fifty mentions of an entity become one, from the session node,
  so an entity held only by old sessions may fall under `semantic.votes` and
  be swept.
- The session node has no text: search reaches it by the graph only.
- Vectors are append-only; the rows of compacted chunks stay on disk.

## What this does not do

- **Search is a brute-force scan** over every chunk vector. `muninn up` takes
  the reading from disk out of a query; the scan itself still grows linearly
  with the bank. An ANN index is what gets added when a bank outgrows it.
- **RRF discards score magnitude.** There is no minimum-similarity gate as in
  the journal and no exact-name floor as in wyrd, so a query with no good answer
  still returns its ten best bad ones. A gate would be a bank parameter.
- **No recency decay.** Time is a filter, not a ranking signal. The journal's
  per-note half-life would come back as a node field and a term in the fusion.
- **PPR runs at node granularity.** A section of a document has no edges of
  its own unless a connector emits sections as nodes.
- **Chunker parameters are per bank, not per node**, and there is no
  kind-to-chunker mapping in the config: a connector that says nothing about
  chunkers gets the bank's default for everything it emits.
- **No bank presets.** A connector has a preset spelling; a bank does not —
  there is no named bundle ("sessions" = claude + codex + entities) to create
  one from. It would be a YAML fragment `bank add --from` reads.
- **The protocol is one-way.** A connector is not told that a record was
  rejected; that appears in the engine's log and its exit status.
- **Text only.** No images, no binary formats; a connector that wants a PDF
  indexed extracts the text itself.
- **No token-budget rendering.** The journal's recall renders into a prompt
  layer; here the client does that with the JSON it gets.

## First consumers

- **Markdown folders** — `connect fs` over `~/life`, notes, docs trees. The
  case that needs engine-side chunking.
- **Coding-agent sessions** — `connect claude` and `connect codex` over the
  tools' own files, then the semantic layer over the result. This is the
  cross-session memory: `muninn search sessions "…" --filter repo=…` from any
  session. A connector over an agent platform's own event log stays possible
  for what only that log knows — tickets, merges, plans — and is not needed for
  the memory itself.
- **wyrd and the note journal** stay as they are. Whether either moves
  onto muninn is decided after it has carried the two cases above.

## Iterations

**Iteration 1** — the stream format and `muninn/stream`; ingest into the on-disk
index with connector ownership, delete, sweep and cursor; the `text` and `none`
chunkers; the Ollama and no-op embedders; dense + BM25 + RRF search; the whole
`bank` and `connector` command set, so no bank is ever made by editing a file;
`index`, `search`; `connect fs`. A markdown bank works end to end,
with edges stored and not yet used.

**Iteration 2** — PPR over the edges with per-kind weights; structural nodes in
results; `node --neighbors`; `index --follow`; `connect claude` and
`connect codex` with the `--preset` spelling, and a `sessions` bank over them.

**Iteration 3** — the semantic layer's write path: patches, `propose` with
undo, regional measurement, the scale-free and fractal checks with the
journal's targets, the budget and singleton checks, all configured through
`bank set`; a `keywords` extractor on top of it, since removed; `semantic reset` and
`semantic log`; `graph stats` and `graph check` over the engine's own metrics.

**Iteration 4** — the `llm` extractor: Ollama, Gemini and Anthropic providers,
candidate lookup against the bank, rejection evidence fed back, intermediate
nodes. The two extractors compared on the sessions bank, and the target bands
re-measured on it.

**Iteration 5** — hybrid banks: `reads` and `edits` edges from the session
connectors under the absolute-path id convention; a bank with sessions and a
project's code together; the `go` chunker. `export --graphml`, archmotif's
cluster metrics in `graph stats`, and the three search-time cuts. The OpenAI embedder; zsh completions.

**Iteration 6** — `muninn ui`: a local page over the same packages the CLI
uses, in the manner of wyrd's. The banks and what `bank show` says about each;
the bank's graph, drawn, with a node's text, attrs, chunks and neighbours on
selection; search in the page, the hits lit on the graph with the mass PPR
gave each, so a ranking can be seen and not only read. It is a process started
by hand and reads the committed snapshot like `search` does; it writes nothing.
