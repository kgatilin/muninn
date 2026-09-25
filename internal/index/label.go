package index

import "strings"

const labelRunes = 60

// Label is something short to show for a node: its title or name attr, the
// attr named as its kind, the
// base name of a path id, otherwise the opening of its text, otherwise the id.
func (n *Node) Label() string {
	// An attr named as the kind is the node's own name: a branch's branch.
	for _, k := range []string{"title", "name", n.Kind} {
		if v := strings.TrimSpace(n.Attrs[k]); v != "" {
			return clip(v)
		}
	}
	if strings.HasPrefix(n.ID, "/") {
		if base := n.ID[strings.LastIndexByte(n.ID, '/')+1:]; base != "" {
			return base
		}
		return n.ID
	}
	if len(n.Chunks) > 0 {
		if t := strings.Join(strings.Fields(n.Chunks[0].Text), " "); t != "" {
			return clip(t)
		}
	}
	return clip(n.ID)
}

func clip(s string) string {
	if r := []rune(s); len(r) > labelRunes {
		return string(r[:labelRunes]) + "…"
	}
	return s
}
