package bank

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Compact is which nodes leave the graph once they are old and every stage
// has read them: what was drawn from them stays, tied to the node they were
// `in`.
type Compact struct {
	// Kinds are the node kinds that go; none switches compaction off.
	Kinds []string `yaml:"kinds,omitempty,flow"`
	// Days is how old the newest node of a container has to be.
	Days int `yaml:"days,omitempty"`
}

func (c Compact) On() bool { return len(c.Kinds) > 0 && c.Days > 0 }

func (c Compact) Compacts(kind string) bool { return slices.Contains(c.Kinds, kind) }

func compactKeys() []string {
	return []string{
		"compact.kinds=<kind>[,<kind>...] (the nodes that go; they are `in` the node that stays)",
		"compact.after=<n>d",
		"compact=off",
	}
}

func (b *Bank) setCompact(key, value string) (note string, err error) {
	switch key {
	case "compact":
		if value != "off" {
			return "", fmt.Errorf("%s: only `off`", key)
		}
		b.Compact = Compact{}
		return "nodes already compacted stay out until their connector is removed and added again", nil
	case "compact.kinds":
		b.Compact.Kinds = list(value)
	case "compact.after":
		days, ok := strings.CutSuffix(value, "d")
		n, err := strconv.Atoi(days)
		if !ok || err != nil || n <= 0 {
			return "", fmt.Errorf("%s: a number of days, as 60d", key)
		}
		b.Compact.Days = n
	default:
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, strings.Join(compactKeys(), "\n  "))
	}
	if !b.Compact.On() {
		return "compaction needs both compact.kinds and compact.after", nil
	}
	return "the next `index` removes the old nodes of these kinds that every stage has read; a later change of prompt or model cannot read them again", nil
}
