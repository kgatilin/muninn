package bank

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kgatilin/muninn/internal/llm"
)

// Semantic is the bank's semantic layer as bank.yaml states it: what proposes
// patches, and the checks every patch passes. What is left out takes the
// default, which Resolved fills in.
type Semantic struct {
	Method string `yaml:"method,omitempty"`
	// Model is the llm method's chat model, <provider> or <provider>:<model>;
	// Endpoint overrides the provider's base URL, and Concurrency is how many
	// passages are with the model at once.
	Model       string `yaml:"model,omitempty"`
	Endpoint    string `yaml:"endpoint,omitempty"`
	Concurrency int    `yaml:"concurrency,omitempty"`
	// Kinds are the node kinds the stage reads; empty is every kind with text.
	// A hybrid bank names its prose here — turn — and leaves its code out.
	Kinds []string `yaml:"kinds,omitempty"`
	// Votes is how many text nodes of a run must choose a name before an
	// entity is minted for it; a name fewer chose ties nothing together.
	Votes int `yaml:"votes,omitempty"`
	// Mentions is how many entities a text node may mention, Rounds how many
	// times a rejected patch is revised, Hops and RegionCap the measured
	// region: the patch's footprint, that many hops out, up to that many nodes.
	Mentions  int              `yaml:"mentions,omitempty"`
	Rounds    int              `yaml:"rounds,omitempty"`
	Hops      int              `yaml:"hops,omitempty"`
	RegionCap int              `yaml:"region_cap,omitempty"`
	Checks    map[string]Check `yaml:"checks,omitempty"`
}

// Check is one configured check: a kind of the catalogue below, switched on or
// off, with the parameters that differ from the kind's defaults.
type Check struct {
	Enabled *bool              `yaml:"enabled,omitempty"`
	Params  map[string]float64 `yaml:"params,omitempty"`
}

type checkKind struct {
	enabled bool
	params  map[string]float64
}

// checkKinds is the catalogue: every kind the gate has, whether it is on in a
// bank that says nothing, and its parameters with their defaults. The bands
// are the starting targets of the note journal muninn was drawn from.
var checkKinds = map[string]checkKind{
	"scale_free": {true, map[string]float64{
		"low": 2.0, "high": 3.5, "ks": 0.1, "rln": 3, "ks_weight": 1, "rln_weight": 0.1,
		"max_regression": 0, "min_nodes": 50, "min_tail": 10,
	}},
	"fractal": {true, map[string]float64{
		"low": 2.5, "high": 4.5, "max_regression": 0, "min_nodes": 50,
		"centres": 64, "radius": 6, "reject_collapse": 1,
	}},
	"entity_budget": {true, map[string]float64{"allowance": 50, "ratio": 0.25, "max_regression": 0}},
	// Off in a bank that says nothing: every entity is a singleton at its first
	// mention, so on a young layer the check refuses whatever would start it.
	"singleton_share": {false, map[string]float64{"ceiling": 0.6, "max_regression": 0, "min_entities": 50}},
}

const (
	SemanticOff = "off"
	SemanticLLM = "llm"
)

// CheckKinds are the catalogue's kinds, sorted.
func CheckKinds() []string {
	kinds := make([]string, 0, len(checkKinds))
	for k := range checkKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// Resolved is the settings with every default filled in and every kind of the
// catalogue present.
func (s Semantic) Resolved() Semantic {
	out := s
	if out.Method == "" {
		out.Method = SemanticOff
	}
	for _, d := range []struct {
		v   *int
		def int
	}{{&out.Mentions, 5}, {&out.Votes, 2}, {&out.Rounds, 3}, {&out.Hops, 2}, {&out.RegionCap, 2000}, {&out.Concurrency, 2}} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	out.Checks = map[string]Check{}
	for kind, def := range checkKinds {
		enabled := def.enabled
		params := map[string]float64{}
		for k, v := range def.params {
			params[k] = v
		}
		if c, ok := s.Checks[kind]; ok {
			if c.Enabled != nil {
				enabled = *c.Enabled
			}
			for k, v := range c.Params {
				params[k] = v
			}
		}
		out.Checks[kind] = Check{Enabled: &enabled, Params: params}
	}
	return out
}

// Reads says whether the stage reads text nodes of the kind.
func (s Semantic) Reads(kind string) bool { return len(s.Kinds) == 0 || slices.Contains(s.Kinds, kind) }

func (s Semantic) validate() error {
	switch s.Method {
	case "", SemanticOff:
	case SemanticLLM:
		if s.Model == "" {
			return fmt.Errorf("semantic.method=llm needs a model: `bank set <bank> semantic.model=<provider>[:<model>]` first; providers: %s", strings.Join(llm.Providers(), ", "))
		}
	default:
		return fmt.Errorf("semantic.method %q; accepted: off, llm", s.Method)
	}
	if s.Model != "" {
		if _, err := llm.ParseSpec(s.Model); err != nil {
			return fmt.Errorf("semantic.model %q: %w", s.Model, err)
		}
	}
	if s.Mentions < 0 || s.Votes < 0 || s.Rounds < 0 || s.Hops < 0 || s.RegionCap < 0 || s.Concurrency < 0 {
		return fmt.Errorf("semantic: mentions, rounds, hops, region_cap and concurrency are positive numbers")
	}
	for _, kind := range s.Kinds {
		if kind == "" || strings.ContainsAny(kind, " \t,") || kind == "entity" || kind == "topic" {
			return fmt.Errorf("semantic.kinds: %q is not a kind the stage can read; a comma list of node kinds, none of them entity or topic", kind)
		}
	}
	for kind, c := range s.Checks {
		def, ok := checkKinds[kind]
		if !ok {
			return fmt.Errorf("unknown check %q; accepted: %s", kind, strings.Join(CheckKinds(), ", "))
		}
		for p := range c.Params {
			if _, ok := def.params[p]; !ok {
				return fmt.Errorf("check %s has no parameter %q; accepted: %s", kind, p, strings.Join(paramNames(def), ", "))
			}
		}
	}
	for kind, c := range s.Resolved().Checks {
		if low, ok := c.Params["low"]; ok && low > c.Params["high"] {
			return fmt.Errorf("check %s: low %g is above high %g", kind, low, c.Params["high"])
		}
	}
	return nil
}

func paramNames(k checkKind) []string {
	names := make([]string, 0, len(k.params))
	for p := range k.params {
		names = append(names, p)
	}
	sort.Strings(names)
	return names
}

func semanticKeys() []string {
	keys := []string{
		"semantic.method=off|llm",
		"semantic.model=<" + strings.Join(llm.Providers(), "|") + ">[:<model>]",
		"semantic.endpoint=<url>",
		"semantic.concurrency=<n>",
		"semantic.kinds=<kind>[,<kind>...] (empty reads every kind)",
		"semantic.mentions=<n>",
		"semantic.votes=<n> (text nodes of a run that must choose a name before it is an entity)",
		"semantic.rounds=<n>",
		"semantic.hops=<n>",
		"semantic.region_cap=<nodes>",
	}
	for _, kind := range CheckKinds() {
		keys = append(keys, "semantic.checks."+kind+".enabled=true|false",
			"semantic.checks."+kind+".<"+strings.Join(paramNames(checkKinds[kind]), "|")+">=<number>")
	}
	return keys
}

// setSemantic applies one semantic.* key. The change is made on a copy and
// kept only when the whole of it validates.
func (b *Bank) setSemantic(parts []string, key, value string) (note string, err error) {
	next := b.Semantic
	next.Checks = map[string]Check{}
	for kind, c := range b.Semantic.Checks {
		params := map[string]float64{}
		for k, v := range c.Params {
			params[k] = v
		}
		next.Checks[kind] = Check{Enabled: c.Enabled, Params: params}
	}
	s := &next
	defer func() {
		if err == nil {
			if len(next.Checks) == 0 {
				next.Checks = nil
			}
			b.Semantic = next
		}
	}()
	number := func(into *int) error {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s: a positive number", key)
		}
		*into = n
		return nil
	}
	switch {
	case key == "semantic.method":
		s.Method = value
		if err := s.validate(); err != nil {
			return "", err
		}
		if value == SemanticOff {
			return "the layer already built stays; `muninn semantic reset` drops it", nil
		}
		return "the next `index` reads every text node the stage has not been over; `muninn semantic reset` first to compare two methods on one bank", nil
	case key == "semantic.model":
		s.Model = value
	case key == "semantic.endpoint":
		s.Endpoint = value
	case key == "semantic.concurrency":
		err = number(&s.Concurrency)
	case key == "semantic.kinds":
		s.Kinds = nil
		for _, kind := range strings.Split(value, ",") {
			if kind = strings.TrimSpace(kind); kind != "" && !slices.Contains(s.Kinds, kind) {
				s.Kinds = append(s.Kinds, kind)
			}
		}
		sort.Strings(s.Kinds)
		if err := s.validate(); err != nil {
			return "", err
		}
		return "the next `index` reads the pending text nodes of these kinds; what other kinds mention already stays until `muninn semantic reset`", nil
	case key == "semantic.mentions":
		err = number(&s.Mentions)
	case key == "semantic.votes":
		err = number(&s.Votes)
	case key == "semantic.rounds":
		err = number(&s.Rounds)
	case key == "semantic.hops":
		err = number(&s.Hops)
	case key == "semantic.region_cap":
		err = number(&s.RegionCap)
	case len(parts) == 4 && parts[1] == "checks":
		kind, param := parts[2], parts[3]
		if _, ok := checkKinds[kind]; !ok {
			return "", fmt.Errorf("unknown check %q; accepted: %s", kind, strings.Join(CheckKinds(), ", "))
		}
		c := s.Checks[kind]
		if param == "enabled" {
			on, err := strconv.ParseBool(value)
			if err != nil {
				return "", fmt.Errorf("%s: true or false", key)
			}
			c.Enabled = &on
		} else {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return "", fmt.Errorf("%s: a number", key)
			}
			if c.Params == nil {
				c.Params = map[string]float64{}
			}
			c.Params[param] = v
		}
		s.Checks[kind] = c
	default:
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(Keys(), "\n  "))
	}
	if err != nil {
		return "", err
	}
	return "", s.validate()
}
