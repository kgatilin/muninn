package bank

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kgatilin/muninn/internal/glob"
)

// Class is what a node is as a source: a guide that holds now, code, the plan
// of a feature long since built, talk. A search ranks within a class and
// answers class by class, so the many nodes of one do not crowd out the few of
// another. A node belongs to the first class that takes it, and the classes
// are answered in their order; a node none takes is of the class its kind
// names.
type Class struct {
	Name string `yaml:"name"`
	// Kinds and Paths are alternatives: a node of one of the kinds, or one whose
	// id matches one of the globs.
	Kinds []string `yaml:"kinds,omitempty,flow"`
	Paths []string `yaml:"paths,omitempty,flow"`
}

type Classes []Class

func (cs Classes) validate() error {
	for _, c := range cs {
		if !nameRE.MatchString(c.Name) {
			return fmt.Errorf("class.%s: lowercase letters, digits, - and _ only", c.Name)
		}
		if _, err := glob.Compile(c.Paths); err != nil {
			return fmt.Errorf("class.%s.paths: %w", c.Name, err)
		}
	}
	return nil
}

// Of returns the function that names a node's class.
func (cs Classes) Of() func(kind, id string) string {
	type compiled struct {
		Class
		match func(string) bool
	}
	all := make([]compiled, len(cs))
	for i, c := range cs {
		res, _ := glob.Compile(c.Paths) // validate has been over them
		all[i] = compiled{c, func(id string) bool { return glob.Match(res, id) }}
	}
	return func(kind, id string) string {
		for _, c := range all {
			if slices.Contains(c.Kinds, kind) || c.match(id) {
				return c.Name
			}
		}
		return kind
	}
}

// Names are the classes in the order they are answered.
func (cs Classes) Names() []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func classKeys() []string {
	return []string{
		"class.<name>.paths=<glob>[,<glob>...] (the nodes whose ids match are of the class)",
		"class.<name>.kinds=<kind>[,<kind>...]",
		"class.<name>=off",
	}
}

func (b *Bank) setClass(parts []string, key, value string) (note string, err error) {
	unknown := fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(classKeys(), "\n  "))
	if len(parts) < 2 || len(parts) > 3 {
		return "", unknown
	}
	next := slices.Clone(b.Classes)
	i := slices.IndexFunc(next, func(c Class) bool { return c.Name == parts[1] })
	if len(parts) == 2 {
		if value != "off" {
			return "", unknown
		}
		if i >= 0 {
			next = slices.Delete(next, i, i+1)
		}
	} else {
		if i < 0 {
			next = append(next, Class{Name: parts[1]})
			i = len(next) - 1
		}
		switch parts[2] {
		case "paths":
			next[i].Paths = list(value)
		case "kinds":
			next[i].Kinds = list(value)
		default:
			return "", unknown
		}
	}
	if err := next.validate(); err != nil {
		return "", err
	}
	b.Classes = next
	return "", nil
}
