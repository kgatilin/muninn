package semantic

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kgatilin/muninn/internal/index"
)

type OpKind string

const (
	OpAddEntity          OpKind = "add_entity"
	OpAddMention         OpKind = "add_mention"
	OpMergeEntities      OpKind = "merge_entities"
	OpSplitEntity        OpKind = "split_entity"
	OpInsertIntermediate OpKind = "insert_intermediate"
	OpDescribe           OpKind = "describe"
)

// Op is one operation of a patch. Which fields it reads depends on its kind;
// the constructors below say which.
type Op struct {
	Kind OpKind `json:"op"`
	// Entity is the entity the operation is about: the one added, mentioned,
	// merged away, split, or stood behind a topic.
	Entity  string   `json:"entity"`
	Name    string   `json:"name,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// Node is the text node of a mention, Weight the edge's weight.
	Node   string  `json:"node,omitempty"`
	Weight float64 `json:"weight,omitempty"`
	// Score is the extractor's confidence in a mention. The layer does not
	// store it; a revision drops the lowest first.
	Score float64 `json:"score,omitempty"`
	// Into is the entity a merge keeps. New is the entity a split makes or the
	// topic an intermediate inserts, named by Name; Nodes are the text nodes
	// that move to it.
	Into  string   `json:"into,omitempty"`
	New   string   `json:"new,omitempty"`
	Nodes []string `json:"nodes,omitempty"`
	// Text is the paragraph a description gives an entity or a topic.
	Text string `json:"text,omitempty"`
}

// Patch is an ordered list of operations, accepted or rejected as one.
type Patch struct {
	Ops []Op `json:"ops"`
}

// Normalize is an entity's name as its id carries it: lowercase, runs of
// whitespace collapsed.
func Normalize(name string) string { return strings.Join(strings.Fields(strings.ToLower(name)), " ") }

func EntityID(name string) string { return "entity:" + Normalize(name) }
func TopicID(name string) string  { return "topic:" + Normalize(name) }

// AddEntity adds an entity by name; one that exists already is left as it is.
func AddEntity(name string, aliases ...string) Op {
	return Op{Kind: OpAddEntity, Entity: EntityID(name), Name: name, Aliases: aliases}
}

// AddMention ties a text node to an entity or topic that exists by then.
func AddMention(node, entity string, weight float64) Op {
	return Op{Kind: OpAddMention, Node: node, Entity: entity, Weight: weight}
}

// MergeEntities moves every edge of from onto into, keeps from's name and
// aliases as aliases of into, and removes from.
func MergeEntities(from, into string) Op { return Op{Kind: OpMergeEntities, Entity: from, Into: into} }

// SplitEntity makes a new entity and moves the mentions of the listed text
// nodes from the old entity to it.
func SplitEntity(entity, name string, nodes ...string) Op {
	return Op{Kind: OpSplitEntity, Entity: entity, New: EntityID(name), Name: name, Nodes: nodes}
}

// InsertIntermediate makes a topic that is part of the entity and moves the
// mentions of the listed text nodes from the entity to the topic.
func InsertIntermediate(entity, name string, nodes ...string) Op {
	return Op{Kind: OpInsertIntermediate, Entity: entity, New: TopicID(name), Name: name, Nodes: nodes}
}

// Describe gives an entity or a topic its paragraph: what it is, by the text
// nodes that mention it. It replaces the one the node had.
func Describe(entity, text string) Op { return Op{Kind: OpDescribe, Entity: entity, Text: text} }

// footprint is every id the patch names, sorted.
func (p Patch) footprint() []string {
	seen := map[string]bool{}
	for _, op := range p.Ops {
		for _, id := range append([]string{op.Entity, op.Node, op.Into, op.New}, op.Nodes...) {
			if id != "" {
				seen[id] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Summary is the patch in one line, for the evidence log.
func (p Patch) Summary() string {
	n := map[OpKind]int{}
	var mentions []string
	for _, op := range p.Ops {
		n[op.Kind]++
		if op.Kind == OpAddMention {
			mentions = append(mentions, strings.TrimPrefix(op.Entity, "entity:"))
		}
	}
	var parts []string
	for _, k := range []OpKind{OpAddEntity, OpAddMention, OpMergeEntities, OpSplitEntity, OpInsertIntermediate, OpDescribe} {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", k, n[k]))
		}
	}
	out := strings.Join(parts, " ")
	if len(mentions) > 0 {
		out += ": " + strings.Join(mentions, ", ")
	}
	return out
}

// entityText is what the engine chunks, embeds and indexes for an entity: its
// name, then its aliases, then the paragraph that describes it, when it has one.
func entityText(name string, aliases []string, summary string) string {
	text := strings.Join(append([]string{name}, aliases...), "\n")
	if summary != "" {
		text += "\n\n" + summary
	}
	return text
}

func aliasesOf(n *index.Node) []string {
	if n.Attrs["aliases"] == "" {
		return nil
	}
	return strings.Split(n.Attrs["aliases"], "\n")
}

func newSemanticNode(id, kind, name string, aliases []string, summary string) *index.Node {
	attrs := map[string]string{"name": name}
	if len(aliases) > 0 {
		attrs["aliases"] = strings.Join(aliases, "\n")
	}
	if summary != "" {
		attrs["summary"] = summary
	}
	return index.NewTextNode(id, kind, index.SemanticConnector, entityText(name, aliases, summary), attrs)
}

func mention(from, to string, w float64) *index.Edge {
	return &index.Edge{From: from, To: to, Kind: EdgeMentions, Weight: w, Connector: index.SemanticConnector}
}

// apply runs one operation through the layer's primitives. An error leaves
// whatever the operation did so far in the undo log, for the caller to roll
// back with the rest.
func (l *Layer) apply(op Op) error {
	semantic := func(id, what string) (*index.Node, error) {
		n := l.state.Nodes[id]
		if !IsSemantic(n) {
			return nil, fmt.Errorf("%s: %s %q is not in the semantic layer", op.Kind, what, id)
		}
		return n, nil
	}
	fresh := func(id string) error {
		if id == "" || op.Name == "" {
			return fmt.Errorf("%s: a name is required", op.Kind)
		}
		if l.state.Nodes[id] != nil {
			return fmt.Errorf("%s: %q exists already", op.Kind, id)
		}
		return nil
	}
	// moveMentions re-points the listed text nodes from the entity to target.
	moveMentions := func(target string) error {
		if len(op.Nodes) == 0 {
			return fmt.Errorf("%s: no text nodes to move", op.Kind)
		}
		for _, id := range op.Nodes {
			old := l.state.Edges[mention(id, op.Entity, 0).Key()]
			if old == nil || old.Connector != index.SemanticConnector {
				return fmt.Errorf("%s: %q does not mention %q", op.Kind, id, op.Entity)
			}
			l.dropEdge(old.Key())
			l.putEdge(mention(id, target, old.Weight))
		}
		return nil
	}

	switch op.Kind {
	case OpAddEntity:
		if op.Entity == "" || op.Name == "" {
			return fmt.Errorf("%s: a name is required", op.Kind)
		}
		if n := l.state.Nodes[op.Entity]; n != nil {
			if !IsSemantic(n) {
				return fmt.Errorf("%s: %q is a connector's node", op.Kind, op.Entity)
			}
			return nil
		}
		l.putNode(newSemanticNode(op.Entity, KindEntity, op.Name, op.Aliases, ""))

	case OpAddMention:
		if !IsText(l.state.Nodes[op.Node]) {
			return fmt.Errorf("%s: %q is not a text node", op.Kind, op.Node)
		}
		if _, err := semantic(op.Entity, "target"); err != nil {
			return err
		}
		if e := mention(op.Node, op.Entity, op.Weight); l.state.Edges[e.Key()] == nil {
			l.putEdge(e)
		}

	case OpMergeEntities:
		from, err := semantic(op.Entity, "entity")
		if err != nil {
			return err
		}
		into, err := semantic(op.Into, "entity")
		if err != nil {
			return err
		}
		if from == into || from.Kind != into.Kind {
			return fmt.Errorf("%s: %q and %q must be two nodes of one kind", op.Kind, op.Entity, op.Into)
		}
		for _, e := range l.edgesOf(op.Entity) {
			moved := *e
			if moved.From == op.Entity {
				moved.From = op.Into
			} else {
				moved.To = op.Into
			}
			l.dropEdge(e.Key())
			// An edge the kept entity has already, or one between the two, is
			// not made twice.
			if moved.From != moved.To && l.state.Edges[moved.Key()] == nil {
				l.putEdge(&moved)
			}
		}
		l.dropNode(op.Entity)
		aliases := aliasesOf(into)
		for _, a := range append([]string{from.Attrs["name"]}, aliasesOf(from)...) {
			if a != "" && a != into.Attrs["name"] && !contains(aliases, a) {
				aliases = append(aliases, a)
			}
		}
		// The kept entity keeps its paragraph; the next description reads both
		// entities' text nodes.
		l.putNode(newSemanticNode(op.Into, into.Kind, into.Attrs["name"], aliases, into.Attrs["summary"]))

	case OpDescribe:
		n, err := semantic(op.Entity, "entity")
		if err != nil {
			return err
		}
		if strings.TrimSpace(op.Text) == "" {
			return fmt.Errorf("%s: a text is required", op.Kind)
		}
		l.putNode(newSemanticNode(op.Entity, n.Kind, n.Attrs["name"], aliasesOf(n), strings.TrimSpace(op.Text)))

	case OpSplitEntity:
		old, err := semantic(op.Entity, "entity")
		if err != nil {
			return err
		}
		if err := fresh(op.New); err != nil {
			return err
		}
		l.putNode(newSemanticNode(op.New, old.Kind, op.Name, op.Aliases, ""))
		return moveMentions(op.New)

	case OpInsertIntermediate:
		if _, err := semantic(op.Entity, "entity"); err != nil {
			return err
		}
		if err := fresh(op.New); err != nil {
			return err
		}
		l.putNode(newSemanticNode(op.New, KindTopic, op.Name, op.Aliases, ""))
		l.putEdge(&index.Edge{From: op.New, To: op.Entity, Kind: EdgePartOf, Connector: index.SemanticConnector})
		return moveMentions(op.New)

	default:
		return fmt.Errorf("unknown operation %q", op.Kind)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
