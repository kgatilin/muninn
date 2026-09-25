// Package bank is a bank's definition: one folder per bank under the muninn
// home, bank.yaml beside its index. The CLI is the only writer of bank.yaml.
package bank

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kgatilin/muninn/internal/chunk"
	"github.com/kgatilin/muninn/internal/embed"
	"github.com/kgatilin/muninn/internal/usage"
)

type Bank struct {
	Name string `yaml:"-"`
	Dir  string `yaml:"-"`

	Embedder   embed.Spec              `yaml:"embedder"`
	Chunker    string                  `yaml:"chunker"`
	Chunkers   map[string]chunk.Params `yaml:"chunkers,omitempty"`
	Connectors []Connector             `yaml:"connectors,omitempty"`
	Search     Search                  `yaml:"search"`
	Semantic   Semantic                `yaml:"semantic,omitempty"`
	Enrich     Enrich                  `yaml:"enrich,omitempty"`
	Compact    Compact                 `yaml:"compact,omitempty"`
	IndexEvery string                  `yaml:"index_every,omitempty"`
	Keys       KeyPatterns             `yaml:"keys,omitempty"`
	Classes    Classes                 `yaml:"classes,omitempty"`
}

type Connector struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command,flow"`
}

type Search struct {
	Seeds       int                `yaml:"seeds"`
	EdgeWeights map[string]float64 `yaml:"edge_weights,omitempty"`
	// DegreeCap is per edge kind: a node with more edges of the kind than this
	// has them left out of the walk.
	DegreeCap map[string]int `yaml:"degree_cap,omitempty"`
	// Mute is node ids the walk does not cross and search does not return.
	Mute []string `yaml:"mute,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Home is MUNINN_HOME, or ~/.muninn.
func Home() (string, error) {
	if h := os.Getenv("MUNINN_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".muninn"), nil
}

func dirOf(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("bank name %q: lowercase letters, digits, - and _ only", name)
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "banks", name), nil
}

// New is a working bank with no flags given.
func New(name string) (*Bank, error) {
	dir, err := dirOf(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("bank %q already exists", name)
	}
	return &Bank{
		Name:     name,
		Dir:      dir,
		Embedder: embed.Spec{Provider: "ollama", Model: embed.DefaultOllamaModel},
		Chunker:  "text",
		Search:   Search{Seeds: 10},
	}, nil
}

func Load(name string) (*Bank, error) {
	dir, err := dirOf(name)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bank.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		names, _ := List()
		return nil, fmt.Errorf("no bank %q; banks: %s", name, orNone(names))
	}
	if err != nil {
		return nil, err
	}
	b := &Bank{Name: name, Dir: dir}
	if err := yaml.Unmarshal(raw, b); err != nil {
		return nil, fmt.Errorf("%s/bank.yaml: %w", dir, err)
	}
	// The keywords method is gone. A bank that named it loads with the stage
	// off; the layer it built stays until `semantic reset`.
	if b.Semantic.Method == "keywords" {
		b.Semantic.Method = ""
	}
	return b, b.Validate()
}

func List() ([]string, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(home, "banks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(home, "banks", e.Name(), "bank.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (b *Bank) IndexDir() string { return filepath.Join(b.Dir, "index") }

func (b *Bank) Validate() error {
	if _, err := embed.New(b.Embedder); err != nil {
		return err
	}
	if _, ok := chunk.Get(b.Chunker); !ok {
		return unknownChunker(b.Chunker)
	}
	for name := range b.Chunkers {
		if _, ok := chunk.Get(name); !ok {
			return unknownChunker(name)
		}
	}
	seen := map[string]bool{}
	for _, c := range b.Connectors {
		if !nameRE.MatchString(c.Name) {
			return fmt.Errorf("connector name %q: lowercase letters, digits, - and _ only", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("connector %q is defined twice", c.Name)
		}
		if len(c.Command) == 0 {
			return fmt.Errorf("connector %q has no command", c.Name)
		}
		seen[c.Name] = true
	}
	if b.Search.Seeds <= 0 {
		return errors.New("search.seeds must be positive")
	}
	for kind, w := range b.Search.EdgeWeights {
		if w < 0 {
			return fmt.Errorf("search.edge_weights.%s must be zero or more", kind)
		}
	}
	for kind, n := range b.Search.DegreeCap {
		if n <= 0 {
			return fmt.Errorf("search.degree_cap.%s must be positive", kind)
		}
	}
	for i, id := range b.Search.Mute {
		if id == "" || slices.Contains(b.Search.Mute[:i], id) {
			return fmt.Errorf("search.mute: %q is empty or listed twice", id)
		}
	}
	if err := b.Enrich.validate(); err != nil {
		return err
	}
	if err := b.Classes.validate(); err != nil {
		return err
	}
	if err := b.Keys.validate(); err != nil {
		return err
	}
	return b.Semantic.validate()
}

// Save validates and writes bank.yaml atomically.
func (b *Bank) Save() error {
	if err := b.Validate(); err != nil {
		return err
	}
	raw, err := yaml.Marshal(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(b.Dir, ".bank.yaml.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(b.Dir, "bank.yaml"))
}

func (b *Bank) Remove() error { return os.RemoveAll(b.Dir) }

// Keys is what Set accepts.
func Keys() []string {
	return append([]string{
		"embedder=<provider>[:<model>]",
		"embedder.endpoint=<url>",
		"chunker=<name>",
		"chunkers.<name>.budget=<runes>",
		"search.seeds=<n>",
		"search.edge_weights.<kind>=<weight>",
		"search.degree_cap.<kind>=<edges> (0 lifts the cap)",
		"search.mute.add=<node id>",
		"search.mute.remove=<node id>",
	}, slices.Concat(enrichKeys(), semanticKeys(), compactKeys(), keysKeys(), classKeys(), everyKeys())...)
}

// Set applies one key=value. The note, when not empty, says what the change
// costs on the next index run.
func (b *Bank) Set(key, value string) (note string, err error) {
	parts := strings.Split(key, ".")
	switch {
	case key == "embedder":
		spec, err := embed.ParseSpec(value)
		if err != nil {
			return "", err
		}
		spec.Endpoint = b.Embedder.Endpoint
		b.Embedder = spec
		return "the next `index` embeds every chunk again under the new embedder; the old vectors stay on disk under their own key", nil
	case key == "embedder.endpoint":
		b.Embedder.Endpoint = value
	case key == "chunker":
		if _, ok := chunk.Get(value); !ok {
			return "", unknownChunker(value)
		}
		b.Chunker = value
		return rechunkNote, nil
	case len(parts) == 3 && parts[0] == "chunkers" && parts[2] == "budget":
		if _, ok := chunk.Get(parts[1]); !ok {
			return "", unknownChunker(parts[1])
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 100 {
			return "", fmt.Errorf("%s: a number of runes, at least 100", key)
		}
		if b.Chunkers == nil {
			b.Chunkers = map[string]chunk.Params{}
		}
		b.Chunkers[parts[1]] = chunk.Params{Budget: n}
		return rechunkNote, nil
	case key == "search.seeds":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("%s: a positive number", key)
		}
		b.Search.Seeds = n
	case len(parts) == 3 && parts[0] == "search" && parts[1] == "edge_weights":
		w, err := strconv.ParseFloat(value, 64)
		if err != nil || w < 0 {
			return "", fmt.Errorf("%s: a weight, zero or more", key)
		}
		if b.Search.EdgeWeights == nil {
			b.Search.EdgeWeights = map[string]float64{}
		}
		b.Search.EdgeWeights[parts[2]] = w
	case len(parts) == 3 && parts[0] == "search" && parts[1] == "degree_cap":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%s: a number of edges, or 0 to lift the cap", key)
		}
		if n == 0 {
			delete(b.Search.DegreeCap, parts[2])
			break
		}
		if b.Search.DegreeCap == nil {
			b.Search.DegreeCap = map[string]int{}
		}
		b.Search.DegreeCap[parts[2]] = n
	// The id is the value, so an id with dots in it — a path — needs no escaping.
	case key == "search.mute.add":
		if value == "" {
			return "", fmt.Errorf("%s: a node id", key)
		}
		if !slices.Contains(b.Search.Mute, value) {
			b.Search.Mute = append(b.Search.Mute, value)
			sort.Strings(b.Search.Mute)
		}
	case key == "search.mute.remove":
		i := slices.Index(b.Search.Mute, value)
		if i < 0 {
			return "", fmt.Errorf("%s: %q is not muted", key, value)
		}
		b.Search.Mute = slices.Delete(b.Search.Mute, i, i+1)
	case parts[0] == "enrich":
		return b.setEnrich(parts, key, value)
	case parts[0] == "semantic":
		return b.setSemantic(parts, key, value)
	case parts[0] == "compact":
		return b.setCompact(key, value)
	case parts[0] == "index":
		return b.setEvery(key, value)
	case parts[0] == "keys":
		return b.setKeys(parts, key, value)
	case parts[0] == "class":
		return b.setClass(parts, key, value)
	default:
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(Keys(), "\n  "))
	}
	return "", nil
}

const rechunkNote = "nodes are chunked again as their connector emits them; a connector that resumes from a cursor needs `index --full`"

func (b *Bank) Connector(name string) (Connector, bool) {
	for _, c := range b.Connectors {
		if c.Name == name {
			return c, true
		}
	}
	return Connector{}, false
}

func (b *Bank) RemoveConnector(name string) bool {
	for i, c := range b.Connectors {
		if c.Name == name {
			b.Connectors = append(b.Connectors[:i], b.Connectors[i+1:]...)
			return true
		}
	}
	return false
}

func unknownChunker(name string) error {
	return fmt.Errorf("unknown chunker %q; accepted: %s", name, strings.Join(chunk.Names(), ", "))
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none yet — `muninn bank add <name>`"
	}
	return strings.Join(names, ", ")
}

// RecordUsage writes a paid call made for this bank to the usage log of the
// muninn home. The log is a report and not the work: a line that cannot be
// written is said on stderr and the call's result stands.
func (b *Bank) RecordUsage(purpose, model string, t usage.Tokens) {
	home, err := Home()
	if err == nil {
		err = usage.Append(home, b.Name, purpose, model, t)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "muninn: usage log: %v\n", err)
	}
}
