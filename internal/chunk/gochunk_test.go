package chunk

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const goFile = `// Package store keeps things.
package store

import "fmt"

// Store holds the rows.
type Store struct {
	rows map[string]string
}

const (
	small = 1
	large = 2
)

// Load reads one row — «строка» — by name.
func (s *Store) Load(name string) (string, error) {
	v, ok := s.rows[name]
	if !ok {
		return "", fmt.Errorf("no row %q", name)
	}
	return v, nil
}

// Put writes one row.
func (s *Store) Put(name, value string) { s.rows[name] = value } // in place
`

func goChunks(t *testing.T, src string, budget int) []Chunk {
	t.Helper()
	c, ok := Get("go")
	if !ok {
		t.Fatal("go chunker is not registered")
	}
	chunks := c.Chunk(src, Params{Budget: budget})
	prev := 0
	for i, ch := range chunks {
		if ch.Start < prev || ch.End <= ch.Start || ch.End > len(src) {
			t.Fatalf("chunk %d spans %d-%d after %d in %d bytes", i, ch.Start, ch.End, prev, len(src))
		}
		if !utf8.ValidString(src[ch.Start:ch.End]) {
			t.Fatalf("chunk %d cuts a rune: %q", i, src[ch.Start:ch.End])
		}
		if strings.TrimSpace(src[prev:ch.Start]) != "" {
			t.Fatalf("lost between chunks %d and %d: %q", i-1, i, src[prev:ch.Start])
		}
		prev = ch.End
	}
	if strings.TrimSpace(src[prev:]) != "" {
		t.Fatalf("lost after the last chunk: %q", src[prev:])
	}
	return chunks
}

func TestGoSmallFileIsOneChunkUnderThePackage(t *testing.T) {
	chunks := goChunks(t, goFile, 1600)
	if len(chunks) != 1 {
		t.Fatalf("a file under the budget is one chunk, got %d", len(chunks))
	}
	if chunks[0].Prefix != "package store" {
		t.Errorf("prefix %q", chunks[0].Prefix)
	}
}

func TestGoDocCommentsStayWithTheirDeclaration(t *testing.T) {
	chunks := goChunks(t, goFile, 120)
	var load *Chunk
	for i, ch := range chunks {
		body := goFile[ch.Start:ch.End]
		if strings.Contains(body, "func (s *Store) Load") {
			load = &chunks[i]
		}
		for _, decl := range []string{"type Store struct", "func (s *Store) Load", "func (s *Store) Put"} {
			doc := map[string]string{"type Store struct": "// Store holds", "func (s *Store) Load": "// Load reads", "func (s *Store) Put": "// Put writes"}[decl]
			if strings.Contains(body, decl) != strings.Contains(body, doc) {
				t.Errorf("%q and its doc comment are in different chunks: %q", decl, body)
			}
		}
	}
	if load == nil {
		t.Fatal("no chunk holds Load")
	}
	if want := "package store › func (s *Store) Load(name string) (string, error)"; load.Prefix != want {
		t.Errorf("prefix of a chunk that is one method:\n got %q\nwant %q", load.Prefix, want)
	}
	if body := goFile[load.Start:load.End]; !strings.Contains(body, "«строка»") {
		t.Errorf("offsets drifted over the non-ASCII comment: %q", body)
	}
	last := goFile[chunks[len(chunks)-1].Start:chunks[len(chunks)-1].End]
	if !strings.HasSuffix(last, "// in place") {
		t.Errorf("a trailing comment left its line: %q", last)
	}
}

func TestGoOversizedFuncIsCutBetweenStatements(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\n// Run does a lot.\nfunc Run(\n\tn int,\n) error {\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "\t// step %d\n\tif n == %d {\n\t\tprintln(\"step\", %d)\n\t}\n", i, i, i)
	}
	b.WriteString("\treturn nil\n}\n\nfunc after() {}\n")
	src := b.String()

	chunks := goChunks(t, src, 300)
	if len(chunks) < 5 {
		t.Fatalf("a 40-statement func under a 300-rune budget is %d chunks", len(chunks))
	}
	pieces := 0
	for _, ch := range chunks {
		body := src[ch.Start:ch.End]
		if strings.Contains(body, "func after") {
			continue
		}
		pieces++
		if want := "package big › func Run( n int, ) error"; ch.Prefix != want {
			t.Errorf("piece prefix %q, want %q", ch.Prefix, want)
		}
		if n := utf8.RuneCountInString(body); n > 300 {
			t.Errorf("piece of %d runes over a 300 budget", n)
		}
		if pieces > 1 {
			if !strings.HasPrefix(body, "// step") && !strings.HasPrefix(body, "return nil") {
				t.Errorf("piece does not open at a statement: %q", body)
			}
			if strings.Count(body, "{") != strings.Count(body, "}") && !strings.HasSuffix(body, "return nil\n}") {
				t.Errorf("piece cuts a statement: %q", body)
			}
		}
	}
}

func TestGoOversizedTypeIsCutAtLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\ntype Wide struct {\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "\tField%d string\n", i)
	}
	b.WriteString("}\n")
	for _, ch := range goChunks(t, b.String(), 200) {
		if ch.Prefix != "package big › type Wide" {
			t.Errorf("prefix %q", ch.Prefix)
		}
	}
}

func TestGoUnparsableFileFallsBackToText(t *testing.T) {
	src := "package broken\n\nfunc (\n\n" + strings.Repeat("words that are not Go. ", 30) + "\n"
	got := goChunks(t, src, 200)
	want := chunksOf(t, src, 200)
	if len(got) == 0 || len(got) != len(want) {
		t.Fatalf("got %d chunks, the text chunker makes %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("chunk %d: %+v, text makes %+v", i, got[i], want[i])
		}
	}
}
