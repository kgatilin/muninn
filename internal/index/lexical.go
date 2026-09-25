package index

import (
	"math"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Ref addresses a chunk: a node and the chunk's position in it.
type Ref struct {
	Node string
	Idx  int
}

type posting struct {
	Doc uint32
	TF  uint16
}

// Lexical is BM25 over chunks, rebuilt whole at every commit.
type Lexical struct {
	Refs     []Ref
	Lens     []uint32
	Avg      float64
	Postings map[string][]posting
}

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Tokenize lowercases and splits on anything that is not a letter or a digit.
// There is no stemming, in any language.
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func buildLexical(s *State) *Lexical {
	ids := make([]string, 0, len(s.Nodes))
	for id := range s.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	lx := &Lexical{Postings: map[string][]posting{}}
	var total uint64
	for _, id := range ids {
		n := s.Nodes[id]
		for i, c := range n.Chunks {
			doc := uint32(len(lx.Refs))
			tf := map[string]int{}
			tokens := Tokenize(filepath.Base(n.ID) + " " + c.EmbedText())
			for _, t := range tokens {
				tf[t]++
			}
			for t, f := range tf {
				lx.Postings[t] = append(lx.Postings[t], posting{Doc: doc, TF: uint16(min(f, math.MaxUint16))})
			}
			lx.Refs = append(lx.Refs, Ref{Node: id, Idx: i})
			lx.Lens = append(lx.Lens, uint32(len(tokens)))
			total += uint64(len(tokens))
		}
	}
	if len(lx.Refs) > 0 {
		lx.Avg = float64(total) / float64(len(lx.Refs))
	}
	return lx
}

// Has says whether any chunk of the bank holds the term.
func (lx *Lexical) Has(term string) bool { return len(lx.Postings[term]) > 0 }

func LoadLexical(dir string) (*Lexical, error) {
	lx := &Lexical{}
	if err := loadGob(filepath.Join(dir, "lexical.gob"), lx); err != nil {
		return nil, err
	}
	return lx, nil
}

// Scored is a chunk and a score, in whichever channel produced it.
type Scored struct {
	Ref   Ref
	Score float64
}

// Search returns every chunk sharing a term with the query, best first.
func (lx *Lexical) Search(query string) []Scored {
	scores := map[uint32]float64{}
	n := float64(len(lx.Refs))
	seen := map[string]bool{}
	for _, t := range Tokenize(query) {
		if seen[t] {
			continue
		}
		seen[t] = true
		posts := lx.Postings[t]
		if len(posts) == 0 {
			continue
		}
		idf := math.Log(1 + (n-float64(len(posts))+0.5)/(float64(len(posts))+0.5))
		for _, p := range posts {
			tf := float64(p.TF)
			norm := tf + bm25K1*(1-bm25B+bm25B*float64(lx.Lens[p.Doc])/lx.Avg)
			scores[p.Doc] += idf * tf * (bm25K1 + 1) / norm
		}
	}
	out := make([]Scored, 0, len(scores))
	for doc, sc := range scores {
		out = append(out, Scored{Ref: lx.Refs[doc], Score: sc})
	}
	SortScored(out)
	return out
}

// SortScored orders best first, ties by address so a ranking is reproducible.
func SortScored(s []Scored) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		if s[i].Ref.Node != s[j].Ref.Node {
			return s[i].Ref.Node < s[j].Ref.Node
		}
		return s[i].Ref.Idx < s[j].Ref.Idx
	})
}
