package semantic

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Gate is the layer with the policy every patch passes: the region's size and
// the enabled checks. One Gate, one immutable policy for the run.
type Gate struct {
	Layer     *Layer
	Checks    []Check
	Hops      int
	RegionCap int
}

// Evidence is what is kept of one proposal, accepted or not.
type Evidence struct {
	Time     time.Time `json:"time"`
	Node     string    `json:"node,omitempty"`
	Round    int       `json:"round"`
	Decision string    `json:"decision"` // accept, reject, or dropped for the last rejection of a node
	Ops      string    `json:"ops"`
	Region   struct {
		Nodes     int  `json:"nodes"`
		Boundary  int  `json:"boundary"`
		Truncated bool `json:"truncated,omitempty"`
	} `json:"region"`
	Checks []Result `json:"checks"`
	Error  string   `json:"error,omitempty"`
}

// Violations are the results that rejected.
func (e Evidence) Violations() []Result {
	var out []Result
	for _, r := range e.Checks {
		if r.Decision == Reject {
			out = append(out, r)
		}
	}
	return out
}

// Propose is the only way a patch reaches the layer. It applies the patch,
// fixes the region over the graph before and after it, measures both sides of
// that one region, and asks every enabled check. Accepted, the patch stays.
// Rejected, or failed half way through, it is undone in reverse order and the
// layer is what it was, with every violation in the evidence.
func (g *Gate) Propose(p Patch) (Evidence, error) {
	l := g.Layer
	ev := Evidence{Time: time.Now().UTC(), Ops: p.Summary(), Decision: Accept}
	if len(p.Ops) == 0 {
		return ev, errors.New("an empty patch")
	}
	l.begin()
	defer l.end()
	for i, op := range p.Ops {
		if err := l.apply(op); err != nil {
			l.rollback()
			ev.Decision, ev.Error = Reject, err.Error()
			return ev, fmt.Errorf("operation %d: %w", i+1, err)
		}
	}
	region := l.region(p.footprint(), g.Hops, g.RegionCap)
	ev.Region.Nodes, ev.Region.Boundary, ev.Region.Truncated = len(region.Nodes), region.Boundary, region.Truncated
	before := Snapshot{View: l.view(region, true), Counts: l.tx.before}
	after := Snapshot{View: l.view(region, false), Counts: l.counts}
	for _, c := range g.Checks {
		r := c.Evaluate(before, after)
		if r.Decision == Reject {
			ev.Decision = Reject
		}
		ev.Checks = append(ev.Checks, r)
	}
	if ev.Decision == Reject {
		l.rollback()
	}
	return ev, nil
}

// Journal is the evidence log, index/semantic.log: one JSON object per line,
// appended and never rewritten.
type Journal struct {
	f *os.File
	w *bufio.Writer
}

func logPath(indexDir string) string { return filepath.Join(indexDir, "semantic.log") }

func OpenJournal(indexDir string) (*Journal, error) {
	f, err := os.OpenFile(logPath(indexDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f, w: bufio.NewWriter(f)}, nil
}

func (j *Journal) Append(ev Evidence) error { return json.NewEncoder(j.w).Encode(ev) }
func (j *Journal) Flush() error             { return j.w.Flush() }

func (j *Journal) Close() error {
	err := j.w.Flush()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadJournal returns the last n records, oldest first; with rejected, only
// the proposals that did not stay. A bank with no log has no records.
func ReadJournal(indexDir string, n int, rejected bool) ([]Evidence, error) {
	f, err := os.Open(logPath(indexDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Evidence
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var ev Evidence
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return out, fmt.Errorf("%s: %w", logPath(indexDir), err)
		}
		if rejected && ev.Decision == Accept {
			continue
		}
		if out = append(out, ev); n > 0 && len(out) > 2*n {
			out = append(out[:0], out[len(out)-n:]...)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}
