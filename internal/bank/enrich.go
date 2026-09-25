package bank

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kgatilin/muninn/internal/glob"
	"github.com/kgatilin/muninn/internal/llm"
)

// Enrich is the enrichment stage as bank.yaml states it: the chat model that
// reads, and the enrichments by name. With no model, or no enrichment, the
// stage is off.
type Enrich struct {
	Model    string                `yaml:"model,omitempty"`
	Endpoint string                `yaml:"endpoint,omitempty"`
	Sets     map[string]Enrichment `yaml:"sets,omitempty"`
}

// Enrichment is one rule of enrichment: which nodes of the graph are read,
// what the model is asked to draw from them, and what the items it draws
// become. What is left out takes the preset's value and then the default,
// which Resolved fills in.
type Enrichment struct {
	// Preset names a prompt shipped with muninn, with its kind and votes.
	Preset string `yaml:"preset,omitempty"`
	// Kinds and Paths select the nodes read: a node of one of the kinds, whose
	// id matches one of the globs. An empty list does not narrow.
	Kinds []string `yaml:"kinds,omitempty"`
	Paths []string `yaml:"paths,omitempty"`
	// Prompt says what an item is. The frame around it — how the nodes are
	// shown, the form of the answer — is the engine's.
	Prompt string `yaml:"prompt,omitempty"`
	// Fields are the strings an item carries beside its text; they become the
	// attrs of its node.
	Fields []string `yaml:"fields,omitempty"`
	// Kind is the kind of the nodes made.
	Kind string `yaml:"kind,omitempty"`
	// Neighbours is how many of the nearest selected nodes are read with a
	// node, Votes how many nodes of those read together must support an item,
	// Ratio the most items kept per selected node.
	Neighbours int     `yaml:"neighbours,omitempty"`
	Votes      int     `yaml:"votes,omitempty"`
	Ratio      float64 `yaml:"ratio,omitempty"`
}

// enrichPresets are the enrichments shipped with muninn. A preset gives no
// paths: where a project keeps its documents is the bank's to say.
var enrichPresets = map[string]Enrichment{
	"process": {
		Kinds: []string{"message", "reply"}, Kind: "rule", Votes: 1,
		Prompt: `The passages are parts of a user's conversations with a coding agent: a message the user wrote ("User:") or a reply of the agent ("Assistant:"), each with the end of what was said just before it. Find the standing rules of how work is done in this project.

A rule is about the way of working and will apply again to other tasks: a process to follow (commit after every change, never push without being asked), a tool, provider or command to use or to avoid, what must never be touched, how to write code, tests, commit messages, documents or replies, a correction of the agent's approach that the user expects to hold from now on. It comes from the user's words, or from what the agent proposed or concluded and the user accepted or built on; what the user rejected is not a rule, and its opposite may be.

The test: would it still apply to a different task in this project a month from now? These are not rules, however firmly they are put: a request for a feature, a fix or a change of the software being built; a decision about the design of one feature; a request of the moment; a question, a fact, approval or thanks.`,
	},
	"decisions": {
		Kinds: []string{"message", "reply"}, Kind: "rule", Votes: 1,
		Prompt: `The passages are parts of a user's conversations with a coding agent: a message the user wrote ("User:") or a reply of the agent ("Assistant:"), each with the end of what was said just before it. The project has no design documents to speak of: what the system is meant to be is decided in these conversations. Find the standing decisions about the system being built.

A decision says what the system does or must never do, how a part of it is modelled, what it is built from and what was ruled out, which trade-off was taken and what was given up for it — with the reason, when one was given. It comes from the user's words, or from what the agent proposed and the user accepted or built on; what the user rejected is a decision too, against it. Write it so that someone changing that part later knows what to keep: name the part, the behaviour, the values.

The test: would a later change that goes against it undo something the user asked for? These are not decisions: how the work is done — commits, tools, style of replies; a step of an implementation, a bug and its fix, a status, a question; an option that was discussed and left open.`,
	},
	"architecture": {
		Kind: "rule", Votes: 2,
		Prompt: `The passages are design documents of a software project: designs, plans, implementation and review reports. Find the architectural rules of the codebase that they establish or rely on.

An architectural rule constrains the structure of the code beyond one feature: what a package or layer is responsible for and what it must not know, which way dependencies point, where a kind of logic lives, a pattern every component of a kind follows, how components are wired, named or laid out, what is generated and what is written by hand.

These are not rules: the steps, tasks, data fields, endpoints or tests of the feature a document is about; a decision that matters only inside that feature; a description of how something works that constrains nothing.`,
	},
}

// EnrichPresets lists the shipped presets, sorted.
func EnrichPresets() []string {
	var names []string
	for name := range enrichPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolved is the enrichment with the preset's values and the defaults in.
func (e Enrichment) Resolved() Enrichment {
	out, p := e, enrichPresets[e.Preset]
	if len(out.Kinds) == 0 {
		out.Kinds = p.Kinds
	}
	if out.Prompt == "" {
		out.Prompt = p.Prompt
	}
	if out.Kind == "" {
		out.Kind = p.Kind
	}
	if out.Kind == "" {
		out.Kind = "rule"
	}
	if out.Votes == 0 {
		out.Votes = max(p.Votes, 1)
	}
	if out.Neighbours == 0 {
		out.Neighbours = 4
	}
	if out.Ratio == 0 {
		out.Ratio = 0.05
	}
	return out
}

// Missing says what the enrichment still needs before it can run, or "". It is
// set one key at a time, so a bank may hold one that is not whole yet; the
// stage leaves it out and `bank show` says why.
func (e Enrichment) Missing() string {
	r := e.Resolved()
	switch {
	case strings.TrimSpace(r.Prompt) == "":
		return "a prompt or a preset (" + strings.Join(EnrichPresets(), ", ") + ")"
	case len(r.Kinds) == 0 && len(r.Paths) == 0:
		return "kinds or paths: it selects every node"
	}
	return ""
}

// Selector says of a node, by its kind and id, whether the enrichment reads it.
func (e Enrichment) Selector() func(kind, id string) bool {
	res, err := glob.Compile(e.Paths)
	return func(kind, id string) bool {
		if err != nil || (len(e.Kinds) > 0 && !slices.Contains(e.Kinds, kind)) {
			return false
		}
		return len(e.Paths) == 0 || glob.Match(res, id)
	}
}

// Selects is Selector for one node.
func (e Enrichment) Selects(kind, id string) bool { return e.Selector()(kind, id) }

var enrichName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (en Enrich) validate() error {
	if en.Model != "" {
		if _, err := llm.ParseSpec(en.Model); err != nil {
			return fmt.Errorf("enrich.model: %w", err)
		}
	}
	for name, e := range en.Sets {
		if !enrichName.MatchString(name) {
			return fmt.Errorf("enrich.%s: a name is lowercase letters, digits, - and _", name)
		}
		if _, ok := enrichPresets[e.Preset]; e.Preset != "" && !ok {
			return fmt.Errorf("enrich.%s.preset %q; presets: %s", name, e.Preset, strings.Join(EnrichPresets(), ", "))
		}
		r := e.Resolved()
		if _, err := glob.Compile(r.Paths); err != nil {
			return fmt.Errorf("enrich.%s.paths: %w", name, err)
		}
		for _, f := range r.Fields {
			if !enrichName.MatchString(f) || f == "text" || f == "from" || f == "type" {
				return fmt.Errorf("enrich.%s.fields: %q is not a field name an item may carry", name, f)
			}
		}
		if r.Neighbours < 0 || r.Votes < 1 || r.Votes > r.Neighbours+1 || r.Ratio <= 0 {
			return fmt.Errorf("enrich.%s: neighbours %d, votes %d, ratio %g do not go together", name, r.Neighbours, r.Votes, r.Ratio)
		}
	}
	return nil
}

func enrichKeys() []string {
	return []string{
		"enrich.model=<" + strings.Join(llm.Providers(), "|") + ">[:<model>] (off switches the stage off)",
		"enrich.endpoint=<url>",
		"enrich.<name>=off (removes the enrichment)",
		"enrich.<name>.preset=" + strings.Join(EnrichPresets(), "|"),
		"enrich.<name>.kinds=<kind>[,<kind>...]",
		"enrich.<name>.paths=<glob>[,<glob>...] (over node ids)",
		"enrich.<name>.prompt=<text>|@<file>",
		"enrich.<name>.fields=<name>[,<name>...]",
		"enrich.<name>.kind=<kind of the nodes made>",
		"enrich.<name>.neighbours=<n>",
		"enrich.<name>.votes=<n>",
		"enrich.<name>.ratio=<items per selected node>",
	}
}

func list(value string) []string {
	var out []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// setEnrich applies one enrich.* key on a copy, kept when the whole validates.
func (b *Bank) setEnrich(parts []string, key, value string) (note string, err error) {
	next := Enrich{Model: b.Enrich.Model, Endpoint: b.Enrich.Endpoint, Sets: map[string]Enrichment{}}
	for name, e := range b.Enrich.Sets {
		next.Sets[name] = e
	}
	switch {
	case key == "enrich.model":
		if value == "off" {
			value = ""
		}
		next.Model = value
	case key == "enrich.endpoint":
		next.Endpoint = value
	case len(parts) == 2:
		if value != "off" {
			return "", fmt.Errorf("%s: only `off`, which removes the enrichment; its settings are enrich.%s.<field>", key, parts[1])
		}
		delete(next.Sets, parts[1])
		note = "the items it made stay until `muninn rules " + b.Name + " --reset --type " + parts[1] + "`"
	case len(parts) == 3:
		e := next.Sets[parts[1]]
		switch parts[2] {
		case "preset":
			e.Preset = value
		case "kinds":
			e.Kinds = list(value)
		case "paths":
			e.Paths = list(value)
		case "prompt":
			if file, ok := strings.CutPrefix(value, "@"); ok {
				raw, err := os.ReadFile(file)
				if err != nil {
					return "", err
				}
				value = string(raw)
			}
			e.Prompt = strings.TrimSpace(value)
		case "fields":
			e.Fields = list(value)
		case "kind":
			e.Kind = value
		case "neighbours", "votes":
			n, err := strconv.Atoi(value)
			if err != nil {
				return "", fmt.Errorf("%s: a number", key)
			}
			if parts[2] == "votes" {
				e.Votes = n
			} else {
				e.Neighbours = n
			}
		case "ratio":
			if e.Ratio, err = strconv.ParseFloat(value, 64); err != nil {
				return "", fmt.Errorf("%s: a number", key)
			}
		default:
			return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(enrichKeys(), "\n  "))
		}
		next.Sets[parts[1]] = e
		note = "the next `index` reads what enrich." + parts[1] + " selects and has not read under these settings"
	default:
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(enrichKeys(), "\n  "))
	}
	if err := next.validate(); err != nil {
		return "", err
	}
	b.Enrich = next
	return note, nil
}
