package search

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/index"
)

// The text form is written for an agent reading it in a context window: one
// locator it can open, where in the document that is, and enough of the text to
// judge the hit. No scores, no repeated path prefix, no blank lines. The hits
// of a class stand under its name, numbered through. A date is the node's: for
// a document, when the file was added. A hit that
// was not a seed and holds its place through the graph walk has `~` before its
// rank; a structural node, which has no text, is one line: rank, id, kind,
// attrs.

type Render struct {
	// Chars is the snippet length in runes; Full prints the whole chunk.
	Chars int
	Full  bool
	// Attrs are the node attributes worth a line in this bank.
	Attrs []string
	// Warning is what went wrong with the ranking. It is the first thing of the
	// answer: the agent reading it is the only one who can tell the user.
	Warning string
}

const DefaultChars = 280

func (r Render) Text(w io.Writer, query string, hits []Hit) {
	if r.Warning != "" {
		fmt.Fprintf(w, "! %s\n! Tell the user this before answering from these hits, and ask them to fix it.\n", r.Warning)
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "no hits")
		return
	}
	base := commonBase(hits)
	if base != "" {
		fmt.Fprintf(w, "%d hits under %s\n", len(hits), base)
	}
	class, headed := "", false
	for _, h := range hits[1:] {
		headed = headed || h.Class != hits[0].Class
	}
	for i, h := range hits {
		id := strings.TrimPrefix(h.Node.ID, base)
		if headed && (i == 0 || h.Class != class) {
			class = h.Class
			fmt.Fprintf(w, "# %s\n", cmp.Or(class, "graph"))
		}
		if Structural(h.Node) {
			fmt.Fprintf(w, "~%d %s [%s]%s\n", i+1, id, h.Node.Kind, attrPairs(h.Node.Attrs))
			continue
		}
		if !h.Seed && h.Mass > 0 {
			fmt.Fprint(w, "~")
		}
		fmt.Fprintf(w, "%d %s%s", i+1, id, lines(h.Chunk))
		if h.Chunk.Prefix != "" {
			fmt.Fprintf(w, " — %s", h.Chunk.Prefix)
		}
		if !h.Node.At.IsZero() {
			fmt.Fprintf(w, " · %s", h.Node.At.Local().Format(time.DateOnly))
		}
		if h.Sources == 1 {
			fmt.Fprint(w, " · 1 source")
		} else if h.Sources > 1 {
			fmt.Fprintf(w, " · %d sources", h.Sources)
		}
		fmt.Fprintln(w)
		for _, k := range r.Attrs {
			if v := h.Node.Attrs[k]; v != "" {
				fmt.Fprintf(w, "  %s: %s\n", k, v)
			}
		}
		if r.Full {
			for _, l := range strings.Split(h.Chunk.Text, "\n") {
				fmt.Fprintf(w, "  %s\n", l)
			}
		} else if s := r.snippet(h.Chunk.Text, query); s != "" {
			fmt.Fprintf(w, "  %s\n", s)
		}
		if len(h.Also) > 0 {
			also := make([]string, len(h.Also))
			for j, c := range h.Also {
				also[j] = lines(c)
			}
			fmt.Fprintf(w, "  also %s\n", strings.Join(also, " "))
		}
	}
}

type jsonHit struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind,omitempty"`
	Class   string            `json:"class,omitempty"`
	Sources int               `json:"sources,omitempty"`
	Lines   []int             `json:"lines,omitempty"`
	Heading string            `json:"heading,omitempty"`
	Text    string            `json:"text"`
	Score   float64           `json:"score"`
	Mass    float64           `json:"mass"`
	Seed    bool              `json:"seed"`
	Also    [][]int           `json:"also,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

func (r Render) JSON(w io.Writer, query string, hits []Hit) error {
	out := make([]jsonHit, len(hits))
	for i, h := range hits {
		jh := jsonHit{ID: h.Node.ID, Kind: h.Node.Kind, Class: h.Class, Sources: h.Sources, Heading: h.Chunk.Prefix, Score: h.Score, Mass: h.Mass, Seed: h.Seed, Attrs: h.Node.Attrs, Text: h.Chunk.Text}
		if !r.Full {
			jh.Text = r.snippet(h.Chunk.Text, query)
		}
		if h.Chunk.Line > 0 {
			jh.Lines = []int{h.Chunk.Line, h.Chunk.EndLine}
		}
		for _, c := range h.Also {
			jh.Also = append(jh.Also, []int{c.Line, c.EndLine})
		}
		out[i] = jh
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	body := map[string]any{"query": query, "hits": out}
	if r.Warning != "" {
		body["warning"] = r.Warning
	}
	return enc.Encode(body)
}

func lines(c index.Chunk) string {
	if c.Line <= 0 {
		return ""
	}
	if c.EndLine <= c.Line {
		return fmt.Sprintf(":%d", c.Line)
	}
	return fmt.Sprintf(":%d-%d", c.Line, c.EndLine)
}

// commonBase is the directory every hit's id shares, printed once instead of
// on every hit. It knows nothing about paths beyond the slash: ids that do not
// start with one, such as turn:claude-code:abc:3, are left whole, and so are
// ids that share only the root.
func commonBase(hits []Hit) string {
	base := hits[0].Node.ID
	for _, h := range hits[1:] {
		for !strings.HasPrefix(h.Node.ID, base) {
			base = base[:len(base)-1]
		}
	}
	base = base[:strings.LastIndexByte(base, '/')+1]
	if len(base) < 2 || base[0] != '/' {
		return ""
	}
	return base
}

// attrPairs is a node's attrs as ` k=v` pairs in key order.
func attrPairs(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, " %s=%s", k, attrs[k])
	}
	return sb.String()
}

// snippet is the window of the chunk holding the most distinct query terms,
// or its opening when none occurs, on one line.
func (r Render) snippet(text, query string) string {
	width := r.Chars
	if width <= 0 {
		width = DefaultChars
	}
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) <= width {
		return string(runes)
	}

	terms := map[string]bool{}
	for _, t := range index.Tokenize(query) {
		terms[stem(t)] = true
	}
	type occurrence struct {
		at   int
		term string
	}
	var found []occurrence
	for i := 0; i < len(runes); {
		if !isWord(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && isWord(runes[j]) {
			j++
		}
		if t := stem(strings.ToLower(string(runes[i:j]))); terms[t] {
			found = append(found, occurrence{i, t})
		}
		i = j
	}

	start, best := 0, 0
	for i, first := range found {
		distinct := map[string]bool{}
		for _, o := range found[i:] {
			if o.at > first.at+width*3/4 {
				break
			}
			distinct[o.term] = true
		}
		if len(distinct) > best {
			best, start = len(distinct), max(0, first.at-width/4)
		}
	}
	start = min(start, len(runes)-width)
	for start > 0 && start < len(runes) && isWord(runes[start-1]) {
		start++ // do not open in the middle of a word
	}
	end := min(start+width, len(runes))
	for end < len(runes) && end > start && isWord(runes[end-1]) && isWord(runes[end]) {
		end--
	}
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// stem is a prefix, enough to place a snippet over an inflected word. It is
// not used for ranking.
func stem(t string) string {
	const keep = 5
	if utf8.RuneCountInString(t) <= keep {
		return t
	}
	return string([]rune(t)[:keep])
}
