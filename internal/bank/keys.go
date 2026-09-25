package bank

import (
	"fmt"
	"regexp"
	"strings"
)

// KeyPatterns are the patterns whose matches in node ids tie nodes together: a ticket
// number that a design folder and a branch both carry. The name is the kind of
// the node a match becomes.
type KeyPatterns map[string]string

func (k KeyPatterns) validate() error {
	for name, pattern := range k {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("keys.%s: lowercase letters, digits, - and _ only", name)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("keys.%s: %w", name, err)
		}
	}
	return nil
}

func keysKeys() []string {
	return []string{"keys.<name>=<regexp> (nodes whose ids hold the same match are tied to one <name> node; `off` removes)"}
}

func (b *Bank) setKeys(parts []string, key, value string) (note string, err error) {
	if len(parts) != 2 || value == "" {
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(keysKeys(), "\n  "))
	}
	next := KeyPatterns{}
	for name, pattern := range b.Keys {
		next[name] = pattern
	}
	if value == "off" {
		delete(next, parts[1])
	} else {
		next[parts[1]] = value
	}
	if err := next.validate(); err != nil {
		return "", err
	}
	b.Keys = next
	return "the next `index` ties the nodes again under the patterns as they stand", nil
}
