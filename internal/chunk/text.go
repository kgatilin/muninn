package chunk

import (
	"strings"
	"unicode/utf8"
)

// text chunks markdown and plain text: blocks — paragraphs, fenced code,
// headings — packed up to the budget, a heading starting a new chunk once the
// current one has some weight. The prefix is the heading path the chunk opens
// under, rooted at the front-matter title when there is one.
type text struct{}

func (text) Name() string { return "text" }
func (text) Version() int { return 1 }

const pathSep = " › "

type block struct {
	start, end int
	runes      int
	level      int // 0 unless a heading
	title      string
}

func (text) Chunk(s string, p Params) []Chunk {
	limit := budget(p)
	body, docTitle := frontMatter(s)

	var (
		out      []Chunk
		stack    [6]string
		cur      = Chunk{Start: -1}
		curRunes int
	)
	path := func() string {
		parts := make([]string, 0, 7)
		if docTitle != "" && docTitle != stack[0] {
			parts = append(parts, docTitle)
		}
		for _, h := range stack {
			if h != "" {
				parts = append(parts, h)
			}
		}
		return strings.Join(parts, pathSep)
	}
	flush := func() {
		if cur.Start >= 0 {
			cur.End = cur.Start + len(strings.TrimRight(s[cur.Start:cur.End], " \t\r\n"))
			if cur.End > cur.Start {
				out = append(out, cur)
			}
		}
		cur, curRunes = Chunk{Start: -1}, 0
	}
	add := func(start, end, runes int) {
		if cur.Start < 0 {
			cur = Chunk{Start: start, Prefix: path()}
		}
		cur.End = end
		curRunes += runes
	}

	for _, b := range blocks(s, body) {
		if b.level > 0 {
			if curRunes >= limit/4 {
				flush()
			}
			stack[b.level-1] = b.title
			for i := b.level; i < len(stack); i++ {
				stack[i] = ""
			}
		}
		if curRunes > 0 && curRunes+b.runes > limit {
			flush()
		}
		if b.runes > limit {
			flush()
			for _, piece := range split(s, b, limit) {
				add(piece.start, piece.end, piece.runes)
				flush()
			}
			continue
		}
		add(b.start, b.end, b.runes)
	}
	flush()
	return out
}

// frontMatter returns where the body starts and the title the front matter
// states, if any.
func frontMatter(s string) (body int, title string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return 0, ""
	}
	pos := strings.IndexByte(s, '\n') + 1
	for pos < len(s) {
		end := strings.IndexByte(s[pos:], '\n')
		next := len(s)
		line := s[pos:]
		if end >= 0 {
			line, next = s[pos:pos+end], pos+end+1
		}
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return next, title
		}
		if v, ok := strings.CutPrefix(line, "title:"); ok {
			title = strings.Trim(strings.TrimSpace(v), `"'`)
		}
		pos = next
	}
	return 0, "" // never closed: it was not front matter
}

func blocks(s string, from int) []block {
	var (
		out     []block
		open    = -1
		inFence bool
	)
	closeAt := func(end int) {
		if open >= 0 {
			out = append(out, block{start: open, end: end, runes: utf8.RuneCountInString(s[open:end])})
			open = -1
		}
	}
	for pos := from; pos < len(s); {
		next := len(s)
		if i := strings.IndexByte(s[pos:], '\n'); i >= 0 {
			next = pos + i + 1
		}
		line := strings.TrimSpace(s[pos:next])
		fence := strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
		switch {
		case inFence:
			if fence {
				inFence = false
			}
		case fence:
			inFence = true
			if open < 0 {
				open = pos
			}
		case line == "":
			closeAt(pos)
		default:
			if level, title := heading(line); level > 0 {
				closeAt(pos)
				out = append(out, block{start: pos, end: next, runes: utf8.RuneCountInString(s[pos:next]), level: level, title: title})
			} else if open < 0 {
				open = pos
			}
		}
		pos = next
	}
	closeAt(len(s))
	return out
}

func heading(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || (line[level] != ' ' && line[level] != '\t') {
		return 0, ""
	}
	return level, strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[level:]), "#"))
}

// split cuts a block larger than the budget at line ends, and a line larger
// than the budget at rune boundaries.
func split(s string, b block, limit int) []block {
	var out []block
	start, runes := b.start, 0
	emit := func(end int) {
		if end > start {
			out = append(out, block{start: start, end: end, runes: runes})
		}
		start, runes = end, 0
	}
	for pos := b.start; pos < b.end; {
		next := b.end
		if i := strings.IndexByte(s[pos:b.end], '\n'); i >= 0 {
			next = pos + i + 1
		}
		n := utf8.RuneCountInString(s[pos:next])
		if runes > 0 && runes+n > limit {
			emit(pos)
		}
		for n > limit {
			cut := pos
			for i := 0; i < limit; i++ {
				_, size := utf8.DecodeRuneInString(s[cut:])
				cut += size
			}
			runes = limit
			emit(cut)
			pos, n = cut, n-limit
		}
		runes += n
		pos = next
	}
	emit(b.end)
	return out
}
