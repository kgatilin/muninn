package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const doc = `---
title: Agent memory
tags: [a, b]
---
# Agent memory

Intro paragraph about memory.

## Recall

` + "```go\n# not a heading\n\nfunc recall() {}\n```" + `

Recall text.

## Writing

Writing text.
`

func chunksOf(t *testing.T, text string, budget int) []Chunk {
	t.Helper()
	c, ok := Get("text")
	if !ok {
		t.Fatal("text chunker is not registered")
	}
	return c.Chunk(text, Params{Budget: budget})
}

func TestTextSkipsFrontMatterAndCoversTheBody(t *testing.T) {
	chunks := chunksOf(t, doc, 1600)
	if len(chunks) != 1 {
		t.Fatalf("a document under the budget is one chunk, got %d", len(chunks))
	}
	body := doc[chunks[0].Start:chunks[0].End]
	if strings.Contains(body, "tags:") {
		t.Errorf("front matter is in the chunk: %q", body)
	}
	if !strings.HasPrefix(body, "# Agent memory") || !strings.HasSuffix(body, "Writing text.") {
		t.Errorf("chunk does not span the body: %q", body)
	}
}

func TestTextHeadingPathIsThePrefix(t *testing.T) {
	section := strings.Repeat("Sentence of filler text. ", 20)
	text := "---\ntitle: Doc\n---\n# Doc\n\n" + section + "\n\n## Recall\n\n" + section + "\n\n### Ranking\n\n" + section + "\n\n## Writing\n\n" + section + "\n"
	chunks := chunksOf(t, text, 600)
	var prefixes []string
	for _, c := range chunks {
		prefixes = append(prefixes, c.Prefix)
	}
	want := []string{"Doc", "Doc › Recall", "Doc › Recall › Ranking", "Doc › Writing"}
	if strings.Join(prefixes, "|") != strings.Join(want, "|") {
		t.Errorf("prefixes = %q, want %q", prefixes, want)
	}
}

func TestTextKeepsAFenceWholeAndIgnoresHeadingsInIt(t *testing.T) {
	for _, c := range chunksOf(t, doc, 1600) {
		if strings.Contains(c.Prefix, "not a heading") {
			t.Errorf("a line inside a fence became a heading: %q", c.Prefix)
		}
	}
}

func TestTextSplitsAnOversizedBlockWithinTheBudget(t *testing.T) {
	long := strings.Repeat("слово ", 400) // one paragraph, one line, multi-byte
	chunks := chunksOf(t, long, 300)
	if len(chunks) < 2 {
		t.Fatalf("expected a split, got %d chunks", len(chunks))
	}
	covered := 0
	for _, c := range chunks {
		body := long[c.Start:c.End]
		if !utf8.ValidString(body) {
			t.Fatalf("a chunk was cut inside a rune")
		}
		if n := utf8.RuneCountInString(body); n > 300 {
			t.Errorf("chunk of %d runes exceeds the budget", n)
		}
		covered += len(body)
	}
	if covered < len(strings.TrimSpace(long))-len(chunks) {
		t.Errorf("chunks cover %d of %d bytes", covered, len(long))
	}
}

func TestNoneIsOneChunk(t *testing.T) {
	c, _ := Get("none")
	if got := c.Chunk(doc, Params{}); len(got) != 1 || got[0].Start != 0 || got[0].End != len(doc) {
		t.Errorf("none = %+v", got)
	}
	if got := c.Chunk("", Params{}); got != nil {
		t.Errorf("a node without text has no chunks, got %+v", got)
	}
}
