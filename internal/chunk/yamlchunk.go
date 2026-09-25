package chunk

import (
	"strings"
	"unicode/utf8"
)

// yamlsrc chunks YAML along its keys, read from the lines alone, so Helm
// templates and files that do not parse chunk the same way. Each document —
// the text between `---` lines — is chunked on its own. A unit is one
// top-level key or list item with everything since the previous one, so a
// comment above a key stays with it and the units cover the document. Units
// that fit are packed up to the budget; a unit larger than the budget is cut
// the same way one level down, along its own keys and items, and at line ends
// when it has none.
//
// The prefix is the document's `kind` and `metadata.name` when it states
// them, then, for a chunk that is one unit or a piece of one, the key path
// down to it, joined as the text chunker joins a heading path. A list item
// is named by its first line.
type yamlsrc struct{}

func init() { register(yamlsrc{}) }

func (yamlsrc) Name() string { return "yaml" }
func (yamlsrc) Version() int { return 1 }

// yunit is one key or list item and what precedes it; body is where the
// lines under its own line start.
type yunit struct {
	start, body, end int
	context          string
}

func (yamlsrc) Chunk(s string, p Params) []Chunk {
	var out []Chunk
	for _, doc := range yamlDocs(s) {
		var path []string
		if root := yamlRoot(s, doc[0], doc[1]); root != "" {
			path = []string{root}
		}
		out = yamlSpan(s, doc[0], doc[0], doc[1], path, budget(p), out)
	}
	return out
}

// yamlDocs are the spans between document markers, the markers left out.
func yamlDocs(s string) [][2]int {
	var docs [][2]int
	start := 0
	for pos := 0; pos < len(s); {
		next := nextLine(s, pos)
		line := strings.TrimRight(s[pos:next], " \t\r\n")
		if line == "---" || line == "..." || strings.HasPrefix(line, "--- ") {
			docs = append(docs, [2]int{start, pos})
			start = next
		}
		pos = next
	}
	docs = append(docs, [2]int{start, len(s)})
	return docs
}

// yamlSpan packs the units of [from, end), found among the lines from body on,
// and cuts the ones over the budget one level down.
func yamlSpan(s string, from, body, end int, path []string, limit int, out []Chunk) []Chunk {
	emit := func(start, end int, context string) {
		start, end = trimSpan(s, start, end)
		if end <= start {
			return
		}
		parts := path
		if context != "" {
			parts = append(parts[:len(parts):len(parts)], context)
		}
		out = append(out, Chunk{Start: start, End: end, Prefix: strings.Join(parts, pathSep)})
	}
	units := yamlUnits(s, from, body, end)
	if len(units) == 0 {
		for _, piece := range split(s, block{start: from, end: end}, limit) {
			emit(piece.start, piece.end, "")
		}
		return out
	}
	open, runes := -1, 0
	flush := func(upto int) {
		if open < 0 {
			return
		}
		context := ""
		if upto-open == 1 {
			context = units[open].context
		}
		emit(units[open].start, units[upto-1].end, context)
		open, runes = -1, 0
	}
	for i, u := range units {
		n := utf8.RuneCountInString(s[u.start:u.end])
		if n > limit {
			flush(i)
			sub := append(path[:len(path):len(path)], u.context)
			out = yamlSpan(s, u.start, u.body, u.end, sub, limit, out)
			continue
		}
		if open >= 0 && runes+n > limit {
			flush(i)
		}
		if open < 0 {
			open = i
		}
		runes += n
	}
	flush(len(units))
	return out
}

// yamlUnits cuts [from, end) at the lines from body on that open a key or a
// list item at the shallowest indent there, each with the comments right
// above it. A list at the indent of the key
// that holds it, as Kubernetes writes one, stays with that key.
func yamlUnits(s string, from, body, end int) []yunit {
	type line struct {
		pos, lead, indent int // lead: where the comments right above the line start
		text              string
	}
	var lines []line
	shallowest, lead := -1, -1
	for pos := body; pos < end; {
		next := min(nextLine(s, pos), end)
		raw := strings.TrimRight(s[pos:next], " \t\r\n")
		text := strings.TrimLeft(raw, " ")
		switch {
		case strings.HasPrefix(text, "#"):
			if lead < 0 {
				lead = pos
			}
		case text != "":
			l := line{pos: pos, lead: pos, indent: len(raw) - len(text), text: text}
			if lead >= 0 {
				l.lead = lead
			}
			lines = append(lines, l)
			if shallowest < 0 || l.indent < shallowest {
				shallowest = l.indent
			}
			lead = -1
		}
		pos = next
	}
	keysThere := false
	for _, l := range lines {
		if l.indent == shallowest && !yamlItem(l.text) && yamlKey(l.text) != "" {
			keysThere = true
		}
	}
	var units []yunit
	for _, l := range lines {
		if l.indent != shallowest {
			continue
		}
		context := yamlKey(l.text)
		if yamlItem(l.text) {
			if keysThere {
				continue
			}
			context = oneLine(l.text)
			if r := []rune(context); len(r) > 60 {
				context = string(r[:60]) + "…"
			}
		}
		if context == "" {
			continue
		}
		start := l.lead
		if len(units) == 0 {
			start = from
		} else {
			units[len(units)-1].end = start
		}
		units = append(units, yunit{start: start, body: nextLine(s, l.pos), end: end, context: context})
	}
	return units
}

// yamlKey is the key a line opens, or "" when it opens none.
func yamlKey(text string) string {
	if strings.HasPrefix(text, "{{") {
		return ""
	}
	i := strings.Index(text, ":")
	for i >= 0 && i+1 < len(text) && text[i+1] != ' ' && text[i+1] != '\t' {
		j := strings.Index(text[i+1:], ":")
		if j < 0 {
			return ""
		}
		i += 1 + j
	}
	if i <= 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(text[:i]), `"'`)
}

func yamlItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

// yamlRoot names a Kubernetes-style document: its kind and metadata.name.
func yamlRoot(s string, start, end int) string {
	var kind, name string
	inMeta, metaIndent := false, -1
	for pos := start; pos < end; {
		next := min(nextLine(s, pos), end)
		raw := strings.TrimRight(s[pos:next], " \t\r\n")
		text := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(text)
		switch {
		case text == "" || strings.HasPrefix(text, "#"):
		case indent == 0:
			inMeta, metaIndent = raw == "metadata:", -1
			if v, ok := strings.CutPrefix(raw, "kind:"); ok {
				kind = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		case inMeta && name == "":
			if metaIndent < 0 {
				metaIndent = indent
			}
			if v, ok := strings.CutPrefix(text, "name:"); ok && indent == metaIndent {
				name = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		pos = next
	}
	return strings.TrimSpace(kind + " " + name)
}

func nextLine(s string, pos int) int {
	if i := strings.IndexByte(s[pos:], '\n'); i >= 0 {
		return pos + i + 1
	}
	return len(s)
}
