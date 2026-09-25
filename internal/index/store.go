// Package index is a bank's index on disk: the graph with its chunks, the
// vectors, and the lexical index. All of it is derived from what connectors
// emitted; only the vectors cost anything to lose, and their content-hash keys
// survive a rebuild.
package index

import (
	"bufio"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Node struct {
	ID, Kind, Hash string
	Connector      string
	Attrs          map[string]string
	At             time.Time
	Recipe         string
	Chunks         []Chunk
}

// Chunk carries its own text: the node's text is not kept beside it.
type Chunk struct {
	Hash          string
	Prefix, Text  string
	Line, EndLine int
}

// EmbedText is what the embedder and the lexical index are given for a chunk.
func (c Chunk) EmbedText() string {
	if c.Prefix == "" {
		return c.Text
	}
	return c.Prefix + "\n\n" + c.Text
}

type Edge struct {
	From, To, Kind string
	Weight         float64
	Connector      string
}

func (e Edge) key() string { return e.From + "\x00" + e.To + "\x00" + e.Kind }

type RunInfo struct {
	At                                time.Time
	Nodes, Changed, Deleted, Embedded int
	Err                               string
}

type State struct {
	Nodes   map[string]*Node
	Edges   map[string]*Edge
	Cursors map[string]string
	Runs    map[string]RunInfo
	// Semantic is which text nodes the semantic stage has been over: node id to
	// the node's hash and the extractor that read it. A node whose entry differs
	// from what it would be now is read again.
	Semantic map[string]string
	// Named is what the llm extractor's first pass was answered for a text node
	// the second has not read yet: the node's hash and the extractor, a line
	// break, the answer. A run that is stopped while naming keeps its answers.
	Named map[string]string
	// Enriched is the same record for the enrichment stage: the enrichment's
	// name and the node id, to what was read of the node and under which
	// settings.
	Enriched map[string]string
	// Compacted is the nodes compaction removed, id to the connector that emitted
	// them: that connector is not heard when it emits them, or an edge of theirs,
	// again. Removing the connector forgets them.
	Compacted map[string]string
	// Judged is the pairs of entities named in different scripts that the merge
	// pass has asked the model about, so that a pair it left apart is not asked
	// about at every run.
	Judged map[string]bool
}

func newState() *State {
	return &State{Nodes: map[string]*Node{}, Edges: map[string]*Edge{}, Cursors: map[string]string{}, Runs: map[string]RunInfo{}, Semantic: map[string]string{}}
}

// LoadState reads the last committed snapshot; a bank never indexed is empty.
func LoadState(dir string) (*State, error) {
	s := newState()
	if err := loadGob(filepath.Join(dir, "state.gob"), s); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *State) save(dir string) error { return saveGob(filepath.Join(dir, "state.gob"), s) }

func loadGob(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := gob.NewDecoder(bufio.NewReaderSize(f, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func saveGob(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := gob.NewEncoder(w).Encode(v); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lock makes `index` the single writer of a bank.
func lock(dir string) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("the bank is being indexed by another process")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Vectors is one vector space of a bank: a flat float32 matrix beside a table
// of the chunk hashes its rows belong to. Append-only; rows are unit length.
type Vectors struct {
	base string
	dims int
	rows map[string]int
	data []float32
}

const vecMagic = "MNV1"

func OpenVectors(dir, key string) (*Vectors, error) {
	v := &Vectors{base: filepath.Join(dir, "vectors", key), rows: map[string]int{}}
	ids, err := os.ReadFile(v.base + ".ids")
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(v.base + ".f32")
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 || string(raw[:4]) != vecMagic {
		return nil, fmt.Errorf("%s.f32: not a muninn vector file", v.base)
	}
	v.dims = int(binary.LittleEndian.Uint32(raw[4:8]))
	hashes := strings.Fields(string(ids))
	// A crash between the two appends leaves one file longer; the shorter wins.
	n := min(len(hashes), (len(raw)-8)/(4*v.dims))
	v.data = make([]float32, n*v.dims)
	for i := range v.data {
		v.data[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[8+4*i:]))
	}
	for i, h := range hashes[:n] {
		v.rows[h] = i
	}
	return v, nil
}

func (v *Vectors) Len() int { return len(v.rows) }

func (v *Vectors) Has(hash string) bool { _, ok := v.rows[hash]; return ok }

func (v *Vectors) Row(hash string) []float32 {
	i, ok := v.rows[hash]
	if !ok {
		return nil
	}
	return v.data[i*v.dims : (i+1)*v.dims]
}

func (v *Vectors) Append(hashes []string, vecs [][]float32) error {
	if len(hashes) == 0 {
		return nil
	}
	if v.dims == 0 {
		v.dims = len(vecs[0])
	}
	if err := os.MkdirAll(filepath.Dir(v.base), 0o755); err != nil {
		return err
	}
	f32, err := os.OpenFile(v.base+".f32", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f32.Close()
	if len(v.rows) == 0 {
		header := append([]byte(vecMagic), 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(header[4:], uint32(v.dims))
		if _, err := f32.WriteAt(header, 0); err != nil {
			return err
		}
	}
	end := int64(8 + 4*v.dims*len(v.rows))
	if err := f32.Truncate(end); err != nil {
		return err
	}
	buf := make([]byte, 0, 4*v.dims*len(vecs))
	for i, vec := range vecs {
		if len(vec) != v.dims {
			return fmt.Errorf("vector %d has %d dimensions, the space has %d", i, len(vec), v.dims)
		}
		for _, x := range vec {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(x))
		}
	}
	if _, err := f32.WriteAt(buf, end); err != nil {
		return err
	}
	if err := f32.Sync(); err != nil {
		return err
	}

	// The ids file is rewritten to the known rows before appending, for the
	// same crash: it may hold hashes whose vectors never landed.
	ordered := make([]string, len(v.rows), len(v.rows)+len(hashes))
	for h, i := range v.rows {
		ordered[i] = h
	}
	ordered = append(ordered, hashes...)
	tmp := v.base + ".ids.tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(ordered, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, v.base+".ids"); err != nil {
		return err
	}
	for i, h := range hashes {
		v.rows[h] = len(v.data)/v.dims + i
	}
	for _, vec := range vecs {
		v.data = append(v.data, vec...)
	}
	return nil
}
