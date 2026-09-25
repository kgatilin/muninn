package semantic

import (
	"strings"
	"unicode/utf8"

	"github.com/kgatilin/muninn/internal/index"
)

// EntityName is a name as both extractors write it, so that they meet on one
// entity id: the bank's tokens joined by single spaces, the last word folded
// onto its singular when the bank holds that too. Empty when nothing is left,
// when it is longer than four words, or when it is a number or a hash. The
// second value is the name before folding, for the entity's aliases; it is
// the name itself when nothing was folded. held says whether the bank has a
// token, index.Lexical.Has; nil folds nothing.
func EntityName(name string, held func(token string) bool) (string, string) {
	tokens := index.Tokenize(name)
	if len(tokens) == 0 || len(tokens) > llmNameWords {
		return "", ""
	}
	for _, t := range tokens {
		if numeric(t) {
			return "", ""
		}
	}
	plain := strings.Join(tokens, " ")
	if utf8.RuneCountInString(plain) < 2 {
		return "", ""
	}
	last := len(tokens) - 1
	tokens[last] = singular(tokens[last], held)
	return strings.Join(tokens, " "), plain
}

// singular folds a simple English plural onto its singular, and only when the
// bank holds the singular as a word of its own: "worktrees" with "worktree"
// in the bank, and neither "status", whose "statu" no text has, nor "class",
// nor "news", whose "new" is a stopword.
// It knows -s, -es after a sibilant and -ies; it leaves alone words under
// four letters and those ending in -ss, -us or -is.
func singular(t string, held func(string) bool) string {
	if held == nil || len(t) < 4 || !strings.HasSuffix(t, "s") {
		return t
	}
	for _, keep := range []string{"ss", "us", "is"} {
		if strings.HasSuffix(t, keep) {
			return t
		}
	}
	var forms []string
	if stem, ok := strings.CutSuffix(t, "ies"); ok {
		forms = append(forms, stem+"y")
	}
	if stem, ok := strings.CutSuffix(t, "es"); ok {
		for _, sib := range []string{"s", "x", "z", "ch", "sh"} {
			if strings.HasSuffix(stem, sib) {
				forms = append(forms, stem)
			}
		}
	}
	forms = append(forms, t[:len(t)-1])
	for _, f := range forms {
		if held(f) && !stopwords[f] {
			return f
		}
	}
	return t
}

// aliasesBut are the forms that differ from the name, in the order given.
func aliasesBut(name string, forms ...string) []string {
	var out []string
	for _, f := range forms {
		if f != name && !contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
