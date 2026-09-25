package chunk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode/utf8"
)

// gosrc chunks a Go file along its top-level declarations. A unit is one
// declaration with everything since the previous one — its doc comment, and
// any comment floating above it — so the units cover the file. Units that fit
// are packed up to the budget; a function larger than the budget is cut
// between the top-level statements of its body, and anything else that large
// at line ends.
//
// The prefix is the package clause, and for a chunk that is one function, or a
// piece of one, its signature on one line after it, joined as the text
// chunker joins a heading path; the pieces of a split type carry `type Name`. A chunk packed from several declarations carries the package
// only: their signatures are in its text.
//
// A file that does not parse is chunked as text.
type gosrc struct{}

func init() { register(gosrc{}) }

func (gosrc) Name() string { return "go" }
func (gosrc) Version() int { return 1 }

// unit is one declaration and what precedes it.
type unit struct {
	start, end int
	context    string // the signature of a func, `type Name` of a single type
	cuts       []int  // where a func's body may be cut: after each statement's line
}

func (gosrc) Chunk(s string, p Params) []Chunk {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", s, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return text{}.Chunk(s, p)
	}
	limit := budget(p)
	pkg := "package " + file.Name.Name
	units := goUnits(s, fset.File(file.Pos()), file)
	if len(units) == 0 { // a package clause and nothing else
		units = []unit{{start: 0, end: len(s)}}
	}

	var (
		out    []Chunk
		open   = -1 // first unit of the chunk being packed
		runes  int
		prefix = func(context string) string {
			if context == "" {
				return pkg
			}
			return pkg + pathSep + context
		}
	)
	emit := func(start, end int, context string) {
		start, end = trimSpan(s, start, end)
		if end > start {
			out = append(out, Chunk{Start: start, End: end, Prefix: prefix(context)})
		}
	}
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
			for _, piece := range u.pieces(s, limit) {
				emit(piece.start, piece.end, u.context)
			}
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

// goUnits lays the declarations end to end: each unit runs from where the
// previous one stopped to the end of its declaration's last line, the first
// from the top of the file, the last to its end.
func goUnits(s string, tf *token.File, file *ast.File) []unit {
	var units []unit
	prev := 0
	for _, d := range file.Decls {
		pos, end := tf.Offset(d.Pos()), lineEnd(s, tf.Offset(d.End()))
		if pos < prev && len(units) > 0 { // two declarations on one line
			last := &units[len(units)-1]
			last.end, last.context, last.cuts = max(last.end, end), "", nil
			prev = last.end
			continue
		}
		u := unit{start: prev, end: end}
		switch d := d.(type) {
		case *ast.FuncDecl:
			u.context = oneLine(s[pos:tf.Offset(d.Type.End())])
			if d.Body != nil {
				for _, st := range d.Body.List {
					if cut := lineEnd(s, tf.Offset(st.End())); cut < end {
						u.cuts = append(u.cuts, cut)
					}
				}
			}
		case *ast.GenDecl:
			if d.Tok == token.TYPE && len(d.Specs) == 1 {
				u.context = "type " + d.Specs[0].(*ast.TypeSpec).Name.Name
			}
		}
		units = append(units, u)
		prev = end
	}
	if len(units) > 0 {
		units[len(units)-1].end = len(s)
	}
	return units
}

// pieces cuts a unit larger than the budget: the segments between a func's
// statement cuts packed up to the budget, and a segment still too large — one
// long statement, a type, a const block — at line ends.
func (u unit) pieces(s string, limit int) []block {
	var (
		out        []block
		start, pos = u.start, u.start
		runes      int
	)
	flush := func() {
		if pos > start {
			out = append(out, block{start: start, end: pos, runes: runes})
		}
		start, runes = pos, 0
	}
	for _, cut := range append(u.cuts, u.end) {
		if cut <= pos {
			continue
		}
		n := utf8.RuneCountInString(s[pos:cut])
		if runes > 0 && runes+n > limit {
			flush()
		}
		if n > limit {
			out = append(out, split(s, block{start: pos, end: cut}, limit)...)
			pos = cut
			start = cut
			continue
		}
		runes += n
		pos = cut
	}
	flush()
	return out
}

// lineEnd is the offset just past the line holding off, so a trailing comment
// stays with what it follows.
func lineEnd(s string, off int) int {
	if off > 0 && off <= len(s) && s[off-1] == '\n' {
		return off
	}
	if i := strings.IndexByte(s[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(s)
}

func trimSpan(s string, start, end int) (int, int) {
	body := s[start:end]
	start += len(body) - len(strings.TrimLeft(body, " \t\r\n"))
	return start, start + len(strings.Trim(body, " \t\r\n"))
}

func oneLine(sig string) string { return strings.Join(strings.Fields(sig), " ") }
