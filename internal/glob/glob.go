// Package glob is the path patterns of the fs connector and of the bank's
// settings: * and ? within a path segment, ** across segments.
package glob

import (
	"fmt"
	"regexp"
	"strings"
)

// Compile turns the patterns into expressions anchored at both ends.
func Compile(globs []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, g := range globs {
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(g); i++ {
			switch {
			case strings.HasPrefix(g[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 2
			case strings.HasPrefix(g[i:], "**"):
				b.WriteString(".*")
				i++
			case g[i] == '*':
				b.WriteString("[^/]*")
			case g[i] == '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", g, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Match says whether any of the patterns matches the path.
func Match(res []*regexp.Regexp, rel string) bool {
	for _, re := range res {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}
