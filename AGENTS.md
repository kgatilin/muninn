# muninn — notes for agents

Indexes text that lives elsewhere as a graph and searches it; an optional
semantic layer of entities sits over the text nodes. `README.md` is how to run
it, `docs/design.md` is why each part is the way it is and what it does not do.
Read the section of `design.md` you are about to change before changing it, and
change it with the code.

## Build and check

```
make build            # bin/muninn
make test             # go test -race ./...
go vet ./...
```

All three pass before a commit, and `gofmt -l internal` is clean.

`muninn up` answers `search` and serves the page from the binary it was started
with. After a rebuild, restart it, or queries and the page run the old code
against new bank settings (an unknown provider, a missing endpoint).

## Layout

| Package | What it holds |
|---|---|
| `stream` | The public connector format: JSON lines of `node`, `edge`, `delete`, `cursor`, `sweep` |
| `internal/bank` | A bank's `bank.yaml`, every `bank set` key, `Home()`, `RecordUsage` |
| `internal/index` | Ingest, chunks, the vector store keyed by content hash, the lexical index, `Session`, compaction |
| `internal/chunk` | Chunkers: `text`, `none`, `go`, `yaml` |
| `internal/embed` | Embedders: `ollama`, `openai`, `vertex`, `noop` |
| `internal/llm` | Chat clients of the semantic stage: `ollama`, `gemini`, `vertex`, `anthropic` |
| `internal/gcp` | Project, location and a bearer token from the application default credentials |
| `internal/usage` | The log of paid calls and the price table |
| `internal/semantic` | The layer: patches, the gate and its checks, the `llm` extractor, the stage |
| `internal/keys` | The keys stage: nodes whose ids hold one match of a bank's pattern tied to the node that match becomes |
| `internal/enrich` | The enrichment stage: nodes selected by a bank's rule, read in neighbourhoods by a chat model, the items settled against those kept |
| `internal/glob` | Path patterns of the fs connector and of an enrichment's selection |
| `internal/search` | Dense + BM25 + RRF, then personalized PageRank over the edges |
| `internal/fsconn`, `internal/sessconn` | The `fs`, `claude` and `codex` connectors |
| `internal/ui` | The read-only page: a JSON API and static files, no build step |
| `internal/cli` | Cobra commands; a file adds its command from `init` |

## Rules of the codebase

- **No SDKs.** Providers are plain `net/http`. The dependencies are cobra and
  yaml; adding one needs a reason that `design.md` records.
- **Settings go through `muninn bank set`.** `bank.yaml` is never edited by
  hand. A new setting is a field, a default in `Resolved()`, a line of the key
  list, a case in `Set`, and a row in `bank show`.
- **Secrets stay in the environment of the run.** Keys and tokens are read when
  a provider is first used and are written nowhere. A bank on a hosted provider
  can be made, shown and searched lexically without its key.
- **The layer is written through the gate only.** An extractor returns a
  `Patch`; `Gate.Propose` measures the region before and after and accepts or
  rolls back. A new kind of change is a new `Op` with its inverse recorded by
  the layer's primitives, never a direct write to `State`.
- **A provider reports its tokens.** `Embed`, `EmbedQuery` and `Complete`
  return `usage.Tokens`; the caller passes them to `Bank.RecordUsage` with a
  purpose. A new call site that skips this is a cost nobody sees. A new model
  gets a row in the price table of `internal/usage`, from the provider's price
  page and not from memory.
- **An extractor's id names its version.** Changing the llm prompts changes what
  a run produces: bump `llmPromptVersion`, and the next `index` reads the bank
  again. `muninn semantic reset <bank>` drops a layer to compare two methods.
- **An enrichment's frame is versioned.** `version` in `internal/enrich` is
  part of what the state records for a node read; bump it with the frame of a
  prompt. A preset's prompt is hashed into the record by itself.
- **Nothing of a real project in a preset.** A preset carries a prompt, kinds
  and votes; paths are a bank's setting.
- **Names are compared as names.** The fold and the merge pass use the vector
  kept under `index.ChunkHash(name)`, not the entity's chunk, which also holds
  aliases and the description.

## Tests

- Every test that makes a bank sets `MUNINN_HOME` to a temp dir; `newBank` and
  `newServer` do it. A test that reaches the real `~/.muninn` is a bug.
- Providers are tested against `httptest` servers, with the request's wire
  format asserted. `gcptest.Setup(t)` stands in for Google credentials and the
  token exchange.
- The semantic extractors are tested with the `noop` embedder and a scripted
  chat client. `noop` vectors are unrelated to each other, so a test that needs
  two close names seeds unit vectors through the `before` hook of `runLLM`.
- Fixtures use invented names. No real people, companies, projects or paths.

## Style

Comments say why and what is left out, in full sentences, at the density of the
file around them. Commit messages are one line: the area, a colon, what is now
true (`semantic: names are compared by the vectors of the names`). One logical change per commit; `design.md` moves with the code it
describes.

The repository is public-ready: no personal or employer context in code, tests,
fixtures, docs or commit messages. Cloud project ids, bank names and file paths
of a real machine belong to the environment, not to the source.

## Measuring a change to the semantic layer

A change to an extractor is judged on a real bank, not by the tests alone:

```
muninn semantic reset <bank> && muninn index <bank>
muninn graph stats <bank>     # entity degrees, γ and d_B of the semantic graph
muninn graph check <bank>     # each check against its band
muninn usage <bank>           # what the run cost
```

What to look at: entities per text node (an entity is a hub, so several times
fewer than text nodes), the share mentioned once (zero under `semantic.votes`),
the text nodes left without an entity, and whether γ and d_B sit in their
bands. The run's log gives the names by vote count, which is what `votes` is
chosen from.
