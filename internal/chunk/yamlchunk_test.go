package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const valuesFile = `# Values for the router.
replicas: 2

image:
  repository: registry.example.com/router
  tag: "1.4.0"

# Agents the router serves.
agents:
  - name: planner
    model: large
    tools: [search, read]
  - name: reviewer
    model: small
    tools: [read]

env:
  LOG_LEVEL: info
  URL: "http://example.com:8080/x"
`

func yamlChunks(t *testing.T, src string, budget int) []Chunk {
	t.Helper()
	c, ok := Get("yaml")
	if !ok {
		t.Fatal("yaml chunker is not registered")
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
		if gap := strings.TrimSpace(src[prev:ch.Start]); gap != "" && gap != "---" {
			t.Fatalf("lost between chunks %d and %d: %q", i-1, i, gap)
		}
		prev = ch.End
	}
	if strings.TrimSpace(src[prev:]) != "" {
		t.Fatalf("lost after the last chunk: %q", src[prev:])
	}
	return chunks
}

func TestYAMLWholeFileFits(t *testing.T) {
	got := yamlChunks(t, valuesFile, 1600)
	if len(got) != 1 || got[0].Prefix != "" {
		t.Fatalf("got %+v, want one chunk with no prefix", got)
	}
}

func TestYAMLLargeKeyIsCutAlongItsItems(t *testing.T) {
	got := yamlChunks(t, valuesFile, 120)
	var prefixes []string
	for _, ch := range got {
		prefixes = append(prefixes, ch.Prefix)
	}
	want := "|agents › - name: planner|agents › - name: reviewer|env"
	if strings.Join(prefixes, "|") != want {
		t.Fatalf("prefixes %q, want %q", strings.Join(prefixes, "|"), want)
	}
	planner := got[1]
	if !strings.HasPrefix(valuesFile[planner.Start:planner.End], "# Agents the router serves.\nagents:") {
		t.Errorf("the comment and the key are not in the first item's chunk: %q", valuesFile[planner.Start:planner.End])
	}
}

func TestYAMLListAtTheIndentOfItsKey(t *testing.T) {
	src := `containers:
- name: app
  image: app:1
- name: sidecar
  image: proxy:2
ports:
- 8080
`
	got := yamlChunks(t, src, 40)
	var prefixes []string
	for _, ch := range got {
		prefixes = append(prefixes, ch.Prefix)
	}
	want := "containers › - name: app|containers › - name: sidecar|ports"
	if strings.Join(prefixes, "|") != want {
		t.Fatalf("prefixes %q, want %q", strings.Join(prefixes, "|"), want)
	}
}

func TestYAMLDocumentsAndTheirNames(t *testing.T) {
	src := `---
# The router.
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: router
  name: router
spec:
  replicas: 2
---
apiVersion: v1
kind: Service
metadata:
  name: router
spec:
  ports:
    - port: 80
`
	got := yamlChunks(t, src, 1600)
	if len(got) != 2 || got[0].Prefix != "Deployment router" || got[1].Prefix != "Service router" {
		t.Fatalf("got %+v, want the Deployment and the Service, one chunk each", got)
	}
	if !strings.HasPrefix(src[got[0].Start:got[0].End], "# The router.") {
		t.Errorf("the first document starts at %q", src[got[0].Start:got[0].End])
	}
}

func TestYAMLTemplatesAndLongLines(t *testing.T) {
	src := "{{- if .Values.enabled }}\nconfig:\n  {{- toYaml .Values.config | nindent 2 }}\n{{- end }}\nblob: " +
		strings.Repeat("x", 300) + "\n"
	got := yamlChunks(t, src, 100)
	for _, ch := range got {
		if n := utf8.RuneCountInString(src[ch.Start:ch.End]); n > 100 {
			t.Errorf("chunk %q of %d runes is over the budget", ch.Prefix, n)
		}
	}
	if c, _ := Get("yaml"); c.Chunk("  \n", Params{}) != nil {
		t.Error("blank text chunked")
	}
}
