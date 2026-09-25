package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/bank"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/index"
	"github.com/kgatilin/muninn/internal/llm"
	"github.com/kgatilin/muninn/internal/usage"
)

const (
	// llmPromptVersion is part of the extractor's id: a changed prompt reads
	// the bank again.
	llmPromptVersion = "3"
	// llmExcerpt is the rune budget of the passage shown to the model, and
	// llmReviseExcerpt the shorter one shown again with a rejection.
	llmExcerpt       = 6000
	llmReviseExcerpt = 1500
	// llmCandidates is how many existing entities and topics a passage is shown,
	// llmDenseChunks how many of its chunks are compared with their vectors.
	llmCandidates  = 24
	llmDenseChunks = 8
	// A rejection shows the llmHubs largest entities of the patch, each with up
	// to llmHubMentions of the passages that mention it.
	llmHubs        = 3
	llmHubMentions = 30
	llmNameWords   = 4
	// mergeCosine is how close two entities' name vectors are before the model
	// is asked whether they are one; mergeBatch pairs go into one question, and
	// mergePairs bounds one run.
	// foldCosine is how close two names' vectors are before the run counts
	// them as one name: spelling variants, a word order, an abbreviation beside
	// its expansion. Related names sit lower — a part and its whole, two tenses
	// — and are left apart.
	foldCosine = 0.94
	// A description reads up to describePassages of the text nodes that
	// mention the entity, describeExcerpt runes of each.
	describePassages = 8
	describeExcerpt  = 700
	mergeCosine      = 0.92
	// crossCosine is the same for two names in different scripts: a name and
	// its translation sit lower than two spellings of one name do.
	crossCosine = 0.86
	mergeBatch  = 10
	mergePairs  = 200
)

// LLM is the extractor with a chat model. The model names what a passage is
// about, is shown the entities and topics the bank has already so that it
// reuses before it mints, and on rejection reads the violations and
// restructures: a narrower target, fewer mentions, a topic inserted under an
// overloaded entity, an entity split. Whatever it answers is validated against
// the graph before it becomes a patch.
type LLM struct {
	client llm.Client
	// save writes the state while the first pass is asking; nil saves nothing.
	save        func() error
	bank        *bank.Bank
	state       *index.State
	lexical     *index.Lexical
	vectors     *index.Vectors
	mentions    int
	minVotes    int
	concurrency int
	log         io.Writer

	view   View
	known  map[string]bool // semantic ids seen; checked against the state when read
	minted map[string]bool // entities this run proposed: what the merge pass looks at
	order  map[string]int  // node id → position in queue
	queue  []*index.Node
	asked  map[string]chan llmAnswer
	read   int

	embedder embed.Embedder
	canon    map[string]string   // a name of the run → the name it folds onto
	votes    map[string]int      // canonical name → the passages of the run that chose it
	forms    map[string][]string // canonical name → the other names folded onto it
}

type llmAnswer struct {
	mentions []llmMention
	err      error
}

// llmMention is one entry of the model's answer: an existing node by id, or a
// new entity by name.
type llmMention struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

func NewLLM(s *index.Session, client llm.Client) *LLM {
	cfg := s.Bank().Semantic.Resolved()
	x := &LLM{
		client: client, bank: s.Bank(), state: s.State(), lexical: s.Lexical(), vectors: s.Vectors(), mentions: cfg.Mentions, minVotes: cfg.Votes, concurrency: cfg.Concurrency, log: s.Log(), embedder: s.Embedder(),
		known: map[string]bool{}, minted: map[string]bool{}, order: map[string]int{}, asked: map[string]chan llmAnswer{},
	}
	for id, n := range x.state.Nodes {
		if IsSemantic(n) {
			x.known[id] = true
		}
	}
	x.save = s.SaveState
	return x
}

// namedEvery is how many answers of the first pass are between two saves.
const namedEvery = 256

// named is the first pass's answer for the node as an earlier run kept it.
func (x *LLM) named(n *index.Node) ([]llmMention, bool) {
	signature, raw, _ := strings.Cut(x.state.Named[n.ID], "\n")
	var mentions []llmMention
	if signature != n.Hash+" "+x.ID() || json.Unmarshal([]byte(raw), &mentions) != nil {
		return nil, false
	}
	return mentions, true
}

func llmID(model string) string { return "llm/" + llmPromptVersion + "/" + model }

func (x *LLM) ID() string { return llmID(x.client.ID()) }

// ExtractorID is the id the bank's extractor would have, without building it:
// what tells a node that has been read from one that has not.
func ExtractorID(cfg bank.Semantic) string {
	if cfg.Resolved().Method == bank.SemanticLLM {
		if spec, err := llm.ParseSpec(cfg.Model); err == nil {
			return llmID(spec.String())
		}
	}
	return ""
}

const mentionsSchema = `{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"},"confidence":{"type":"number"}},"required":["id","name","confidence"],"additionalProperties":false}}`

const extractSchema = `{"type":"object","properties":{"mentions":` + mentionsSchema + `},"required":["mentions"],"additionalProperties":false}`

const extractSystem = `You name what a passage is about, for a knowledge graph that ties passages to entities and topics through "mentions" edges.

Passages are in English or Russian. Return the few things this passage is really about, the ones a reader would search for: a system, a component, a person, a project, a concept, a decision. Skip generic words (agent, config, file, code, error), dates, numbers, paths and hashes.

Rules:
- At most %d mentions, the most central first. Name the specific things and also the broader subject they belong to (a project, a field, a product): a name only this passage would use ties it to nothing, and is dropped.
- First decide what the passage is about, then look at the candidates listed under it. They are entities the graph has already, found by word overlap; most passages are about things that are not in the list.
- When a candidate names the same thing as one of yours, reuse it: put its id in "id" and leave "name" empty. When two candidates fit, take the narrower one. A candidate the passage only mentions in passing is not reused.
- Everything else is minted: put the new name in "name" and leave "id" empty. A name is in the passage's own language, lowercase, singular, 1 to 4 words, no punctuation. Use the name other passages about the same thing would use: the established term, not a description made up for this passage. A bare generic word ("check") is not a name.
- "confidence" is between 0 and 1: how central the thing is to the passage.
- A passage with nothing worth naming gets an empty list.

Answer with JSON only, following this schema:
` + extractSchema

const reviseSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["mentions","insert_intermediate","split_entity","give_up"]},"mentions":` + mentionsSchema +
	`,"entity":{"type":"string"},"name":{"type":"string"},"nodes":{"type":"array","items":{"type":"string"}}},"required":["action","mentions","entity","name","nodes"],"additionalProperties":false}`

const reviseSystem = `A patch you proposed for a knowledge graph was rejected by a structural gate. The graph ties passages to entities and topics through "mentions" edges. The gate measures the region around a patch before and after it, and rejects a patch that makes the structure worse: an entity attached to everything, which flattens the graph; more entities than the passages can carry; too many entities with a single mention.

Read the violations and restructure. Choose one action:
- "mentions": a revised list for the passage. Attach to a narrower existing entity or topic instead of a large one, drop the weak mentions, mint fewer entities. "id" reuses an existing node, "name" mints an entity.
- "insert_intermediate": put a topic between an overloaded entity and some of the passages that mention it. "entity" is the entity's id, "name" the topic's name, "nodes" the ids of the passages, taken from the entity's list below, that belong under the topic. This passage's own mention of the entity moves to the topic. "mentions" may stay empty to keep the rejected list otherwise.
- "split_entity": the entity names two different things. "entity", the "name" of the new entity, and "nodes": the passages that mean the new one.
- "give_up": the passage keeps no semantic edges.

Use only ids that are shown to you. Names are in the passage's own language (English or Russian), lowercase, singular, 1 to 4 words. Fields the action does not use stay empty.

Answer with JSON only, following this schema:
` + reviseSchema

const mergeSchema = `{"type":"object","properties":{"same":{"type":"array","items":{"type":"object","properties":{"pair":{"type":"integer"},"keep":{"type":"string"}},"required":["pair","keep"],"additionalProperties":false}}},"required":["same"],"additionalProperties":false}`

const mergeSystem = `You are shown pairs of entities from a knowledge graph whose names are close. Names are in English or Russian. For each pair decide whether the two names mean one thing: spelling variants, singular and plural, a translation, an abbreviation and its expansion. Two related things are not one thing: a part and its whole, two versions, two tools of one kind.

Return only the pairs that are one thing, each with "keep": the id of the entity whose name is the better canonical one. The other becomes its alias. When unsure, leave the pair out.

Answer with JSON only, following this schema:
` + mergeSchema

// Prepare is the first pass, over every passage the run will read: the model
// names each, several at a time, and the names are then counted over the run.
// Names whose vectors are within foldCosine are one name, the one more
// passages chose; an entity of the layer takes a name folded onto it. Extract
// mints an entity only for a name the bank's `votes` passages chose, so a name one
// passage uses costs no budget and leaves no singleton.
//
// A request that fails stops the asking and not the pass: the passages
// answered are read and committed, and the stage stops at the first that was
// not, where it can be resumed.
func (x *LLM) Prepare(ctx context.Context, nodes []*index.Node, view View) error {
	x.view = view
	x.queue, x.order = nodes, map[string]int{}
	for i, n := range nodes {
		x.order[n.ID] = i
	}
	answers := make([]llmAnswer, len(nodes))
	// What is kept of the first pass is for the nodes still pending: an entry
	// of a node the second pass has read since is dropped here.
	if x.state.Named == nil {
		x.state.Named = map[string]string{}
	}
	for id := range x.state.Named {
		if _, pending := x.order[id]; !pending {
			delete(x.state.Named, id)
		}
	}
	kept := make([]bool, len(nodes))
	resumed := 0
	for i, n := range nodes {
		if mentions, ok := x.named(n); ok {
			answers[i], kept[i] = llmAnswer{mentions: mentions}, true
			resumed++
		}
	}
	if resumed > 0 {
		fmt.Fprintf(x.log, "semantic llm: %d/%d passages named by an earlier run\n", resumed, len(nodes))
	}
	system := fmt.Sprintf(extractSystem, 2*x.mentions)
	users := make([]string, len(nodes))
	for i, n := range nodes {
		users[i] = "Passage: " + n.ID + "\n" + excerpt(n, llmExcerpt) + "\n\n" + x.candidates(n)
	}
	askCtx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		done   = resumed
		asked  int
		failed error // the first request that failed; the ones it stopped fail with it
	)
	sem := make(chan struct{}, max(x.concurrency, 1))
	for i := range nodes {
		if kept[i] {
			continue
		}
		if askCtx.Err() != nil {
			answers[i].err = askCtx.Err()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var out struct {
				Mentions []llmMention `json:"mentions"`
			}
			err := x.complete(askCtx, usage.Extract, system, users[i], extractSchema, &out)
			answers[i] = llmAnswer{mentions: out.Mentions, err: err}
			mu.Lock()
			if err == nil {
				raw, _ := json.Marshal(out.Mentions)
				x.state.Named[nodes[i].ID] = nodes[i].Hash + " " + x.ID() + "\n" + string(raw)
				if asked++; asked%namedEvery == 0 && x.save != nil {
					if err := x.save(); err != nil && failed == nil {
						failed = err
						stop()
					}
				}
			}
			if err != nil && !soft(err) && failed == nil {
				failed = err
				stop()
			}
			if done++; done%50 == 0 {
				fmt.Fprintf(x.log, "semantic llm: %d/%d passages named\n", done, len(nodes))
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if asked > 0 && x.save != nil {
		if err := x.save(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, n := range nodes {
		if answers[i].err != nil && !soft(answers[i].err) {
			answers[i] = llmAnswer{err: failed}
		}
		ch := make(chan llmAnswer, 1)
		ch <- answers[i]
		x.asked[n.ID] = ch
	}
	return x.count(ctx, nodes, answers)
}

// count folds the names of the run and counts the passages behind each.
func (x *LLM) count(ctx context.Context, nodes []*index.Node, answers []llmAnswer) error {
	chosen := map[string]map[string]bool{} // name → the passages that chose it
	for i, n := range nodes {
		for _, m := range answers[i].mentions {
			if strings.TrimSpace(m.ID) != "" {
				continue
			}
			if name, _ := EntityName(m.Name, x.lexical.Has); name != "" {
				if chosen[name] == nil {
					chosen[name] = map[string]bool{}
				}
				chosen[name][n.ID] = true
			}
		}
	}
	var held []string
	for id, n := range x.state.Nodes {
		if IsSemantic(n) && n.Kind == KindEntity && x.known[id] {
			held = append(held, strings.TrimPrefix(id, "entity:"))
		}
	}
	names := append([]string{}, held...)
	for name := range chosen {
		names = append(names, name)
	}
	if err := x.embedNames(ctx, names); err != nil {
		return err
	}
	x.canon = foldNames(held, chosen, func(name string) []float32 { return x.vectors.Row(index.ChunkHash(name)) })
	x.votes, x.forms = map[string]int{}, map[string][]string{}
	voters := map[string]map[string]bool{}
	for name, into := range x.canon {
		if voters[into] == nil {
			voters[into] = map[string]bool{}
		}
		for id := range chosen[name] {
			voters[into][id] = true
		}
		if name != into {
			x.forms[into] = append(x.forms[into], name)
		}
	}
	minted, folded := 0, 0
	var voted []string
	for into, ids := range voters {
		x.votes[into] = len(ids)
		sort.Strings(x.forms[into])
		if len(ids) >= x.minVotes && x.state.Nodes[EntityID(into)] == nil {
			voted = append(voted, into)
		}
	}
	atLeast := map[int]int{}
	for _, n := range x.votes {
		for v := 2; v <= min(n, 6); v++ {
			atLeast[v]++
		}
	}
	fmt.Fprintf(x.log, "semantic llm: names by the passages that chose them: 2+ %d, 3+ %d, 4+ %d, 5+ %d, 6+ %d\n", atLeast[2], atLeast[3], atLeast[4], atLeast[5], atLeast[6])
	// The entity budget is spent on the names the most passages chose, not on
	// the ones the run happens to read first: past the room it leaves, a name
	// is as good as unchosen.
	sort.Slice(voted, func(i, j int) bool {
		if a, b := x.votes[voted[i]], x.votes[voted[j]]; a != b {
			return a > b
		}
		return voted[i] < voted[j]
	})
	if room := x.room(); room >= 0 && len(voted) > room {
		fmt.Fprintf(x.log, "semantic llm: %d names have the votes and the entity budget has room for %d; the least chosen are left out\n", len(voted), room)
		for _, name := range voted[room:] {
			x.votes[name] = 0
		}
		voted = voted[:room]
	}
	minted = len(voted)
	for name, into := range x.canon {
		if name != into {
			folded++
		}
	}
	fmt.Fprintf(x.log, "semantic llm: %d names over %d passages, %d folded onto another, %d to be minted with %d votes or more\n", len(chosen), len(nodes), folded, minted, x.minVotes)
	return nil
}

// embedNames gives every name a vector under the name's own hash. An entity's
// chunk is its name with its aliases and its description, which is another
// text; names are compared as names.
func (x *LLM) embedNames(ctx context.Context, names []string) error {
	missing := map[string]string{}
	for _, name := range names {
		if h := index.ChunkHash(name); !x.vectors.Has(h) {
			missing[h] = name
		}
	}
	return embedPhrases(ctx, x.bank, x.embedder, x.vectors, missing)
}

// room is how many entities the bank's budget check still allows, or -1 when
// the check is off.
func (x *LLM) room() int {
	cfg := x.bank.Semantic.Resolved()
	check := cfg.Checks["entity_budget"]
	if check.Enabled == nil || !*check.Enabled {
		return -1
	}
	texts, entities := 0, 0
	for _, n := range x.state.Nodes {
		switch {
		case Reads(cfg, n):
			texts++
		case IsSemantic(n) && n.Kind == KindEntity:
			entities++
		}
	}
	return max(0, int(check.Params["allowance"]+check.Params["ratio"]*float64(texts))-entities)
}

// foldNames maps every name of the run onto the name it counts under. Names
// are taken the most chosen first, after the entities the layer holds, and a
// name within foldCosine of one taken before it folds onto that one.
func foldNames(held []string, chosen map[string]map[string]bool, vector func(string) []float32) map[string]string {
	names := make([]string, 0, len(chosen))
	for name := range chosen {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := len(chosen[names[i]]), len(chosen[names[j]]); a != b {
			return a > b
		}
		if a, b := len(names[i]), len(names[j]); a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	sort.Strings(held)
	type taken struct {
		name string
		vec  []float32
	}
	var kept []taken
	isHeld := map[string]bool{}
	for _, name := range held {
		isHeld[name] = true
		kept = append(kept, taken{name, vector(name)})
	}
	canon := map[string]string{}
	for _, name := range names {
		if isHeld[name] {
			canon[name] = name
			continue
		}
		vec, into, best := vector(name), name, foldCosine
		for _, k := range kept {
			if vec == nil || k.vec == nil {
				continue
			}
			if cos := cosine(vec, k.vec); cos >= best {
				into, best = k.name, cos
			}
		}
		canon[name] = into
		if into == name {
			kept = append(kept, taken{name, vec})
		}
	}
	return canon
}

// ask sends the node and the ones after it in the run, up to the bank's
// concurrency. Candidates are gathered here, on the stage's goroutine, where
// nothing writes the graph; only the HTTP call runs beside the gate.
func (x *LLM) ask(ctx context.Context, node *index.Node) chan llmAnswer {
	ahead := []*index.Node{node}
	if at, ok := x.order[node.ID]; ok {
		ahead = x.queue[at:min(at+max(x.concurrency, 1), len(x.queue))]
	}
	for _, n := range ahead {
		if x.asked[n.ID] != nil {
			continue
		}
		system := fmt.Sprintf(extractSystem, x.mentions)
		user := "Passage: " + n.ID + "\n" + excerpt(n, llmExcerpt) + "\n\n" + x.candidates(n)
		ch := make(chan llmAnswer, 1)
		x.asked[n.ID] = ch
		go func() {
			var out struct {
				Mentions []llmMention `json:"mentions"`
			}
			err := x.complete(ctx, usage.Extract, system, user, extractSchema, &out)
			ch <- llmAnswer{mentions: out.Mentions, err: err}
		}()
	}
	return x.asked[node.ID]
}

// errInvalidAnswer is an answer that is not the JSON asked for.
var errInvalidAnswer = errors.New("the answer is not the JSON asked for")

// complete asks once more when the answer does not parse, then gives up on it.
func (x *LLM) complete(ctx context.Context, purpose, system, user, schema string, out any) error {
	var parseErr error
	for range 2 {
		raw, tokens, err := x.client.Complete(ctx, system, user, json.RawMessage(schema))
		x.bank.RecordUsage(purpose, x.client.ID(), tokens)
		if err != nil {
			return err
		}
		if parseErr = json.Unmarshal([]byte(stripFence(raw)), out); parseErr == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: %v", errInvalidAnswer, parseErr)
}

// stripFence takes a JSON answer out of a markdown code fence, which a model
// without constrained decoding likes to add.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// soft says whether an error is one passage's and not the run's: the model
// refused or answered something that is not the JSON. A failed request is the
// run's, and stops the stage where it can be resumed.
func soft(err error) bool { return errors.Is(err, errInvalidAnswer) || errors.Is(err, llm.ErrNoAnswer) }

// Extract asks the model what the passage is about and turns the valid part of
// its answer into a patch. An answer with nothing valid in it is an empty patch.
func (x *LLM) Extract(ctx context.Context, node *index.Node, view View) (Patch, error) {
	x.view = view
	ch := x.ask(ctx, node)
	var ans llmAnswer
	select {
	case ans = <-ch:
	case <-ctx.Done():
		return Patch{}, ctx.Err()
	}
	delete(x.asked, node.ID)
	if x.read++; x.read%10 == 0 {
		fmt.Fprintf(x.log, "semantic llm: %d passages with the model so far\n", x.read)
	}
	if ans.err != nil {
		if soft(ans.err) {
			fmt.Fprintf(x.log, "semantic llm: %s: %v; no patch\n", node.ID, ans.err)
			return Patch{}, nil
		}
		return Patch{}, ans.err
	}
	p, _ := x.patch(node, ans.mentions, false)
	return p, nil
}

// patch turns a mention list into operations. An entry is valid when its id is
// a node of the layer, or its name normalizes to one to four words; a name
// that is an entity or a topic already reuses it. strict fails the whole list
// on one invalid entry; otherwise the entry is left out.
func (x *LLM) patch(node *index.Node, mentions []llmMention, strict bool) (Patch, bool) {
	sort.SliceStable(mentions, func(i, j int) bool { return mentions[i].Confidence > mentions[j].Confidence })
	var adds, ties []Op
	seen := map[string]bool{}
	for _, m := range mentions {
		target := strings.TrimSpace(m.ID)
		name, plain := EntityName(m.Name, x.lexical.Has)
		if into := x.canon[name]; into != "" && into != name {
			name, plain = into, into
		}
		switch {
		case target != "" && IsSemantic(x.state.Nodes[target]):
		case name != "" && IsSemantic(x.state.Nodes[EntityID(name)]):
			target = EntityID(name)
		case name != "" && IsSemantic(x.state.Nodes[TopicID(name)]):
			target = TopicID(name)
		case name != "" && x.state.Nodes[EntityID(name)] == nil && (target == "" || !strict):
			// A name too few passages of the run chose is not minted.
			if x.votes[name] < x.minVotes {
				if strict {
					return Patch{}, false
				}
				continue
			}
			target = EntityID(name)
			if !seen[target] {
				adds = append(adds, AddEntity(name, aliasesBut(name, append([]string{plain}, x.forms[name]...)...)...))
			}
		default:
			if strict {
				return Patch{}, false
			}
			continue
		}
		if seen[target] || len(ties) == x.mentions {
			continue
		}
		seen[target] = true
		tie := AddMention(node.ID, target, 0)
		tie.Score = m.Confidence
		ties = append(ties, tie)
	}
	// An entity added for a mention the cap left out is not added.
	var p Patch
	for _, op := range adds {
		if seen[op.Entity] {
			p.Ops = append(p.Ops, op)
			x.known[op.Entity], x.minted[op.Entity] = true, true
		}
	}
	p.Ops = append(p.Ops, ties...)
	return p, true
}

// excerpt is what the model reads of a node. A node within the budget is read
// whole, each heading prefix once. A longer one is its outline — the distinct
// prefixes, within a quarter of the budget — and then its leading text.
func excerpt(n *index.Node, budget int) string {
	total := 0
	for _, c := range n.Chunks {
		total += utf8.RuneCountInString(c.Text)
	}
	var b strings.Builder
	// write adds a line within what is left of a budget; false when it is spent.
	write := func(s string, left *int) bool {
		if r := []rune(s); len(r) > *left {
			b.WriteString(string(r[:*left]) + "…\n")
			*left = 0
			return false
		}
		b.WriteString(s + "\n")
		*left -= utf8.RuneCountInString(s) + 1
		return *left > 0
	}
	whole := total <= budget
	if !whole {
		outline := budget / 4
		budget -= outline
		b.WriteString("Outline:\n")
		last := ""
		for _, c := range n.Chunks {
			if c.Prefix != "" && c.Prefix != last {
				if last = c.Prefix; !write("- "+strings.ReplaceAll(c.Prefix, "\n", " / "), &outline) {
					break
				}
			}
		}
		b.WriteString("\nLeading text:\n")
	}
	last := ""
	for _, c := range n.Chunks {
		if whole && c.Prefix != "" && c.Prefix != last {
			last = c.Prefix
			b.WriteString("[" + strings.ReplaceAll(c.Prefix, "\n", " / ") + "]\n")
		}
		if !write(c.Text, &budget) {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// candidates are the entities and topics of the layer closest to the node, as
// the model is shown them. Two channels over the live graph, which the index
// on disk does not hold yet: lexical — every word of a name or an alias occurs
// in the node — and dense — the cosine of the name's vector to the node's
// leading chunks. An entity minted since the last commit has no vector and is
// found by its words alone.
func (x *LLM) candidates(n *index.Node) string {
	words := map[string]bool{}
	for _, c := range n.Chunks {
		for _, t := range index.Tokenize(c.EmbedText()) {
			words[t] = true
		}
	}
	type scored struct {
		id    string
		score float64
	}
	var found []scored
	for id := range x.known {
		sn := x.state.Nodes[id]
		if !IsSemantic(sn) {
			delete(x.known, id)
			continue
		}
		var score float64
		for _, name := range append([]string{sn.Attrs["name"]}, aliasesOf(sn)...) {
			tokens := index.Tokenize(name)
			hit := len(tokens) > 0
			for _, t := range tokens {
				hit = hit && words[t]
			}
			if hit {
				score = max(score, 2+0.1*float64(len(tokens)))
			}
		}
		if len(sn.Chunks) > 0 {
			if ev := x.vectors.Row(sn.Chunks[0].Hash); ev != nil {
				best := 0.0
				for _, c := range n.Chunks[:min(len(n.Chunks), llmDenseChunks)] {
					best = max(best, cosine(ev, x.vectors.Row(c.Hash)))
				}
				score += best
			}
		}
		if score > 0 {
			found = append(found, scored{id, score})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].score != found[j].score {
			return found[i].score > found[j].score
		}
		return found[i].id < found[j].id
	})
	if len(found) == 0 {
		return "Candidates already in the graph: none yet."
	}
	var b strings.Builder
	b.WriteString("Candidates already in the graph (id | name | passages mentioning it):\n")
	for _, f := range found[:min(len(found), llmCandidates)] {
		b.WriteString(x.describe(f.id) + "\n")
	}
	return strings.TrimSpace(b.String())
}

func (x *LLM) describe(id string) string {
	n := x.state.Nodes[id]
	name := n.Attrs["name"]
	if a := aliasesOf(n); len(a) > 0 {
		name += " (also: " + strings.Join(a, ", ") + ")"
	}
	degree := 0
	if x.view != nil {
		degree = x.view.Degree(id)
	}
	return fmt.Sprintf("%s | %s | %d", id, name, degree)
}

// cosine of two unit vectors; zero when either is missing or they differ in size.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return float64(dot)
}

// Revise shows the model the rejected patch, every violation and the largest
// entities the patch touched with the passages that mention them, and turns
// its answer into the next patch. An answer that names an unknown id, has an
// empty name or moves a passage that does not mention the entity is an invalid
// proposal: the round falls back to dropping the weakest mention.
func (x *LLM) Revise(ctx context.Context, node *index.Node, rejected Patch, violations []Result, round int) (Patch, bool) {
	var out struct {
		Action   string       `json:"action"`
		Mentions []llmMention `json:"mentions"`
		Entity   string       `json:"entity"`
		Name     string       `json:"name"`
		Nodes    []string     `json:"nodes"`
	}
	if err := x.complete(ctx, usage.Revise, reviseSystem, x.rejection(node, rejected, violations), reviseSchema, &out); err != nil {
		if ctx.Err() != nil {
			return Patch{}, false
		}
		fmt.Fprintf(x.log, "semantic llm: %s round %d: %v; dropping the weakest mention\n", node.ID, round, err)
		return dropWeakest(rejected, violations)
	}
	next, err := x.revision(node, rejected, out.Action, out.Mentions, out.Entity, out.Name, out.Nodes)
	if err != nil {
		fmt.Fprintf(x.log, "semantic llm: %s round %d: invalid proposal (%v); dropping the weakest mention\n", node.ID, round, err)
		return dropWeakest(rejected, violations)
	}
	return next, len(next.Ops) > 0
}

func (x *LLM) revision(node *index.Node, rejected Patch, action string, mentions []llmMention, entity, name string, nodes []string) (Patch, error) {
	if action == "give_up" {
		return Patch{}, nil
	}
	// The mention list: the model's, or the rejected one where it sent none.
	var base Patch
	if len(mentions) > 0 {
		var ok bool
		if base, ok = x.patch(node, mentions, true); !ok {
			return Patch{}, errors.New("a mention names no node of the layer and no valid name")
		}
	} else {
		for _, op := range rejected.Ops {
			if op.Kind == OpAddEntity || op.Kind == OpAddMention {
				base.Ops = append(base.Ops, op)
			}
		}
	}
	switch action {
	case "mentions":
		if len(mentions) == 0 {
			return Patch{}, errors.New("no mentions")
		}
		if base.Summary() == rejected.Summary() {
			return Patch{}, errors.New("the same patch again")
		}
		return base, nil
	case "insert_intermediate", "split_entity":
	default:
		return Patch{}, fmt.Errorf("unknown action %q", action)
	}

	old := x.state.Nodes[entity]
	if !IsSemantic(old) || old.Kind != KindEntity {
		return Patch{}, fmt.Errorf("%q is not an entity of the layer", entity)
	}
	name, _ = EntityName(name, x.lexical.Has)
	if name == "" {
		return Patch{}, errors.New("the new node has no valid name")
	}
	var moved []string
	seen := map[string]bool{}
	for _, id := range nodes {
		if e := x.state.Edges[mention(id, entity, 0).Key()]; e == nil || e.Connector != index.SemanticConnector {
			return Patch{}, fmt.Errorf("%q does not mention %q", id, entity)
		}
		if !seen[id] {
			seen[id] = true
			moved = append(moved, id)
		}
	}
	if len(moved) == 0 {
		return Patch{}, errors.New("no passages to move")
	}
	op := InsertIntermediate(entity, name, moved...)
	if action == "split_entity" {
		op = SplitEntity(entity, name, moved...)
	}
	if x.state.Nodes[op.New] != nil {
		return Patch{}, fmt.Errorf("%q exists already", op.New)
	}
	x.known[op.New] = true
	// The passage's own mention of the entity goes to the new node.
	p := Patch{Ops: []Op{op}}
	for _, o := range base.Ops {
		if o.Kind == OpAddMention && o.Entity == entity {
			o.Entity = op.New
		}
		p.Ops = append(p.Ops, o)
	}
	return p, nil
}

// rejection is the user prompt of a revision.
func (x *LLM) rejection(node *index.Node, rejected Patch, violations []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Passage: %s\n%s\n\nThe rejected patch:\n", node.ID, excerpt(node, llmReviseExcerpt))
	minted := map[string]bool{}
	var hubs []string
	for _, op := range rejected.Ops {
		switch op.Kind {
		case OpAddEntity:
			minted[op.Entity] = true
		case OpAddMention:
			if minted[op.Entity] {
				fmt.Fprintf(&b, "- mention of a new entity %q\n", strings.TrimPrefix(op.Entity, "entity:"))
			} else if IsSemantic(x.state.Nodes[op.Entity]) {
				fmt.Fprintf(&b, "- mention of %s\n", x.describe(op.Entity))
				hubs = append(hubs, op.Entity)
			}
		case OpInsertIntermediate, OpSplitEntity:
			fmt.Fprintf(&b, "- %s: %q under %s, moving %d passages\n", op.Kind, op.Name, op.Entity, len(op.Nodes))
		}
	}
	b.WriteString("\nViolations:\n")
	for _, v := range violations {
		fmt.Fprintf(&b, "- %s: %s (loss %.3f → %.3f)\n", v.Check, v.Reason, v.LossBefore, v.LossAfter)
	}
	if x.view == nil {
		return b.String()
	}
	sort.SliceStable(hubs, func(i, j int) bool { return x.view.Degree(hubs[i]) > x.view.Degree(hubs[j]) })
	for _, id := range hubs[:min(len(hubs), llmHubs)] {
		if x.state.Nodes[id].Kind != KindEntity || x.view.Degree(id) < 2 {
			continue
		}
		fmt.Fprintf(&b, "\nEntity %s\n", x.describe(id))
		shown := 0
		for _, v := range x.view.Neighbors(id) {
			switch vn := x.state.Nodes[v]; {
			case IsSemantic(vn) && vn.Kind == KindTopic:
				fmt.Fprintf(&b, "  topic under it: %s\n", x.describe(v))
			case IsText(vn) && shown < llmHubMentions:
				shown++
				fmt.Fprintf(&b, "  passage %s — %s\n", v, firstLine(vn))
			}
		}
	}
	return b.String()
}

// firstLine is the start of a node's text, to tell passages apart by more than
// their ids.
func firstLine(n *index.Node) string {
	text := strings.Join(strings.Fields(n.Chunks[0].Text), " ")
	if r := []rune(text); len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return text
}

// Finish is the merge pass, run once after the text nodes are read and the
// new entities embedded. Every entity this run minted is compared with every
// other by the cosine of their name vectors; the pairs above mergeCosine go to
// the model in small batches, and a confirmed pair is a MergeEntities patch for
// the gate. Pairs of two older entities were judged by the run that minted the
// later one and are not asked about again.
//
// Two names in different scripts — a name and its translation — are paired
// from crossCosine, whichever run minted them, and each such pair is asked
// about once: a query in one language then reaches, through the entity, what
// was written in the other. A pair the model left apart is not asked about
// again; one it made one and the gate refused is.
func (x *LLM) Finish(ctx context.Context, view View) ([]Patch, error) {
	x.view = view
	entity := func(id string) *index.Node {
		if n := x.state.Nodes[id]; IsSemantic(n) && n.Kind == KindEntity {
			return n
		}
		return nil
	}
	var names []string
	for id := range x.known {
		if n := entity(id); n != nil {
			names = append(names, n.Attrs["name"])
		}
	}
	if err := x.embedNames(ctx, names); err != nil {
		return nil, err
	}
	vector := func(id string) []float32 {
		if n := entity(id); n != nil {
			return x.vectors.Row(index.ChunkHash(n.Attrs["name"]))
		}
		return nil
	}
	all := make([]string, 0, len(x.known))
	for id := range x.known {
		if vector(id) != nil {
			all = append(all, id)
		}
	}
	sort.Strings(all)
	type pair struct {
		a, b string
		cos  float64
		// cross is the pair's key in State.Judged, for names in different scripts.
		cross string
	}
	scripts := make(map[string]string, len(all))
	for _, id := range all {
		scripts[id] = script(entity(id).Attrs["name"])
	}
	var pairs []pair
	for _, a := range all {
		for _, b := range all {
			if a == b {
				continue
			}
			if scripts[a] != scripts[b] {
				key := a + "\x00" + b
				if a < b && !x.state.Judged[key] {
					if cos := cosine(vector(a), vector(b)); cos >= crossCosine {
						pairs = append(pairs, pair{a, b, cos, key})
					}
				}
				continue
			}
			if !x.minted[a] || (x.minted[b] && b < a) {
				continue
			}
			if cos := cosine(vector(a), vector(b)); cos >= mergeCosine {
				pairs = append(pairs, pair{a: a, b: b, cos: cos})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].cos > pairs[j].cos })
	pairs = pairs[:min(len(pairs), mergePairs)]

	var patches []Patch
	gone := map[string]bool{}
	for start := 0; start < len(pairs); start += mergeBatch {
		batch := pairs[start:min(start+mergeBatch, len(pairs))]
		var b strings.Builder
		for i, p := range batch {
			fmt.Fprintf(&b, "Pair %d:\n  %s\n  %s\n", i+1, x.describe(p.a), x.describe(p.b))
		}
		var out struct {
			Same []struct {
				Pair int    `json:"pair"`
				Keep string `json:"keep"`
			} `json:"same"`
		}
		if err := x.complete(ctx, usage.Merge, mergeSystem, b.String(), mergeSchema, &out); err != nil {
			if soft(err) {
				fmt.Fprintf(x.log, "semantic llm: merge pass: %v; the batch is left as it is\n", err)
				continue
			}
			return patches, err
		}
		for _, p := range batch {
			if p.cross != "" {
				if x.state.Judged == nil {
					x.state.Judged = map[string]bool{}
				}
				x.state.Judged[p.cross] = true
			}
		}
		for _, s := range out.Same {
			if s.Pair < 1 || s.Pair > len(batch) {
				continue
			}
			p := batch[s.Pair-1]
			// The model names the one to keep by its id, or by the id without
			// its kind.
			keeps := func(id string) bool {
				_, name, _ := strings.Cut(id, ":")
				return s.Keep == id || s.Keep == name
			}
			from, into := p.a, p.b
			if keeps(p.a) {
				from, into = p.b, p.a
			} else if !keeps(p.b) {
				continue
			}
			// An entity merged away by an earlier pair is not merged twice.
			if gone[from] || gone[into] {
				continue
			}
			gone[from] = true
			// A pair the model made one is settled by the merge; when the gate
			// refuses the merge, the pair comes up again at the next run.
			delete(x.state.Judged, p.cross)
			patches = append(patches, Patch{Ops: []Op{MergeEntities(from, into)}})
		}
	}
	if len(pairs) > 0 {
		fmt.Fprintf(x.log, "semantic llm: merge pass: %d close pairs shown, %d confirmed\n", len(pairs), len(patches))
	}
	described, err := x.describeAll(ctx, gone, mergedInto(patches))
	return append(patches, described...), err
}

// script is the script a name is written in, when it is not Latin: the first
// such script among its letters.
func script(name string) string {
	for _, r := range name {
		if !unicode.IsLetter(r) || unicode.Is(unicode.Latin, r) {
			continue
		}
		for s, table := range unicode.Scripts {
			if unicode.Is(table, r) {
				return s
			}
		}
	}
	return ""
}

// mergedInto is, per kept entity, the entities the merge patches fold into it.
func mergedInto(patches []Patch) map[string][]string {
	out := map[string][]string{}
	for _, p := range patches {
		for _, op := range p.Ops {
			if op.Kind == OpMergeEntities {
				out[op.Into] = append(out[op.Into], op.Entity)
			}
		}
	}
	return out
}

const describeSchema = `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`

const describeSystem = `You write the description of one entity of a knowledge graph built over a collection of notes. You are given the entity's name and excerpts of the notes that mention it.

Write one paragraph of two to four sentences: what the thing is, and what these notes say about it or do with it. Say only what the excerpts support. Write in the language most of the excerpts are in. Start with the thing itself, not with "this entity" or "the notes".

Answer with JSON only, following this schema:
` + describeSchema

// describeAll writes the paragraph of every entity this run minted or merged
// into, and of any that has none yet, from the text nodes that mention it. An
// entity is a name until then; the paragraph is what a reader, and the search,
// find under it. Entities merged away are skipped, and the one that keeps
// their mentions is described from both. One that the model does not describe
// stays a name.
func (x *LLM) describeAll(ctx context.Context, gone map[string]bool, merged map[string][]string) ([]Patch, error) {
	var ids []string
	for id := range x.known {
		n := x.state.Nodes[id]
		if !IsSemantic(n) || n.Kind != KindEntity || gone[id] {
			continue
		}
		if x.minted[id] || len(merged[id]) > 0 || n.Attrs["summary"] == "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	users := make([]string, len(ids))
	for i, id := range ids {
		users[i] = x.evidence(id, merged[id])
	}
	texts := make([]string, len(ids))
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed error
	)
	sem := make(chan struct{}, max(x.concurrency, 1))
	for i := range ids {
		if users[i] == "" || ctx.Err() != nil {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var out struct {
				Summary string `json:"summary"`
			}
			err := x.complete(ctx, usage.Describe, describeSystem, users[i], describeSchema, &out)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				texts[i] = strings.Join(strings.Fields(out.Summary), " ")
			case soft(err):
				fmt.Fprintf(x.log, "semantic llm: %s: %v; no description\n", ids[i], err)
			case failed == nil:
				failed = err
				stop()
			}
		}()
	}
	wg.Wait()
	var patches []Patch
	for i, id := range ids {
		if texts[i] != "" {
			patches = append(patches, Patch{Ops: []Op{Describe(id, texts[i])}})
		}
	}
	if len(ids) > 0 {
		fmt.Fprintf(x.log, "semantic llm: %d of %d entities described\n", len(patches), len(ids))
	}
	// The descriptions written before a request failed are kept.
	return patches, failed
}

// evidence is what the model reads to describe an entity: its name and
// aliases, and the part of each text node around the name. also are entities
// about to be merged into it, whose text nodes count as its own.
func (x *LLM) evidence(id string, also []string) string {
	n := x.state.Nodes[id]
	forms := append([]string{n.Attrs["name"]}, aliasesOf(n)...)
	seen := map[string]bool{}
	var texts []*index.Node
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		for _, v := range x.view.Neighbors(id) {
			vn := x.state.Nodes[v]
			switch {
			case vn == nil || seen[v]:
			case IsText(vn) && !IsSemantic(vn):
				seen[v] = true
				texts = append(texts, vn)
			case IsSemantic(vn) && vn.Kind == KindTopic && depth == 0:
				seen[v] = true
				walk(v, 1)
			}
		}
	}
	for _, e := range append([]string{id}, also...) {
		if o := x.state.Nodes[e]; o != nil && e != id {
			forms = append(forms, o.Attrs["name"])
		}
		walk(e, 0)
	}
	if len(texts) == 0 {
		return ""
	}
	sort.Slice(texts, func(i, j int) bool { return texts[i].ID < texts[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "Entity: %s\n", n.Attrs["name"])
	if len(forms) > 1 {
		fmt.Fprintf(&b, "Also written: %s\n", strings.Join(forms[1:], ", "))
	}
	fmt.Fprintf(&b, "Mentioned by %d notes; excerpts of %d:\n", len(texts), min(len(texts), describePassages))
	for _, t := range texts[:min(len(texts), describePassages)] {
		fmt.Fprintf(&b, "\n--- %s\n%s\n", t.ID, around(t, forms, describeExcerpt))
	}
	return b.String()
}

// around is the node's text from the chunk that first names one of the forms,
// within the budget; a node that names none is read from its start.
func around(n *index.Node, forms []string, budget int) string {
	start := 0
scan:
	for i, c := range n.Chunks {
		lower := strings.ToLower(c.Text)
		for _, f := range forms {
			if f != "" && strings.Contains(lower, strings.ToLower(f)) {
				start = i
				break scan
			}
		}
	}
	var b strings.Builder
	for _, c := range n.Chunks[start:] {
		for _, r := range c.Text {
			if budget == 0 {
				return b.String() + "…"
			}
			b.WriteRune(r)
			budget--
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}
