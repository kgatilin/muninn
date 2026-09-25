// Package stream is the graph stream: the one input contract of muninn.
//
// A connector is any process that writes these records to stdout, one JSON
// object per line. docs/design.md, "The graph stream", is the specification;
// this package is the Go rendering of it, so a connector in Go is a loop and
// a few Writer calls.
package stream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Node is a vertex. With Text it is searchable; without, it is structural and
// exists to carry edges.
type Node struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
	Text string `json:"text,omitempty"`
	// Hash covers Text. Left empty, the engine computes it.
	Hash  string            `json:"hash,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
	// Chunker names the chunking strategy for this node; empty means the
	// bank's default.
	Chunker string     `json:"chunker,omitempty"`
	At      *time.Time `json:"at,omitempty"`
}

// Edge may name a node that has not arrived yet.
type Edge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight,omitempty"`
}

// Delete removes a node and every edge touching it.
type Delete struct {
	ID string `json:"id"`
}

// Cursor is an opaque checkpoint, handed back in MUNINN_CURSOR on the
// connector's next start.
type Cursor struct {
	Value string `json:"value"`
}

// Sweep says the run was complete: whatever this connector produced before
// and did not produce in this run is gone.
type Sweep struct{}

// Record is one line of the stream. Exactly one field is set.
type Record struct {
	Node   *Node   `json:"node,omitempty"`
	Edge   *Edge   `json:"edge,omitempty"`
	Delete *Delete `json:"delete,omitempty"`
	Cursor *Cursor `json:"cursor,omitempty"`
	Sweep  *Sweep  `json:"sweep,omitempty"`
}

// Validate reports a record that sets no field, several, or an empty identity.
func (r Record) Validate() error {
	n := 0
	for _, set := range []bool{r.Node != nil, r.Edge != nil, r.Delete != nil, r.Cursor != nil, r.Sweep != nil} {
		if set {
			n++
		}
	}
	switch {
	case n != 1:
		return fmt.Errorf("record must set exactly one of node, edge, delete, cursor, sweep; it sets %d", n)
	case r.Node != nil && r.Node.ID == "":
		return errors.New("node without id")
	case r.Edge != nil && (r.Edge.From == "" || r.Edge.To == "" || r.Edge.Kind == ""):
		return errors.New("edge needs from, to and kind")
	case r.Delete != nil && r.Delete.ID == "":
		return errors.New("delete without id")
	}
	return nil
}

// Writer writes records as JSON lines.
type Writer struct {
	buf *bufio.Writer
	enc *json.Encoder
}

func NewWriter(w io.Writer) *Writer {
	buf := bufio.NewWriterSize(w, 1<<16)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	return &Writer{buf: buf, enc: enc}
}

func (w *Writer) Node(n Node) error      { return w.enc.Encode(Record{Node: &n}) }
func (w *Writer) Edge(e Edge) error      { return w.enc.Encode(Record{Edge: &e}) }
func (w *Writer) Delete(id string) error { return w.enc.Encode(Record{Delete: &Delete{ID: id}}) }
func (w *Writer) Sweep() error           { return w.enc.Encode(Record{Sweep: &Sweep{}}) }

// Cursor writes a checkpoint and flushes, so the engine can commit up to it.
func (w *Writer) Cursor(value string) error {
	if err := w.enc.Encode(Record{Cursor: &Cursor{Value: value}}); err != nil {
		return err
	}
	return w.buf.Flush()
}

func (w *Writer) Flush() error { return w.buf.Flush() }

// Reader reads records; Next returns io.EOF at the end of the stream.
type Reader struct {
	dec *json.Decoder
	n   int
}

func NewReader(r io.Reader) *Reader { return &Reader{dec: json.NewDecoder(r)} }

func (r *Reader) Next() (Record, error) {
	var rec Record
	if err := r.dec.Decode(&rec); err != nil {
		if errors.Is(err, io.EOF) {
			return rec, io.EOF
		}
		return rec, fmt.Errorf("record %d: %w", r.n+1, err)
	}
	r.n++
	if err := rec.Validate(); err != nil {
		return rec, fmt.Errorf("record %d: %w", r.n, err)
	}
	return rec, nil
}
