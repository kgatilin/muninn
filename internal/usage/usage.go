// Package usage is the record of the paid model calls: one JSON line per call
// in usage.jsonl under the muninn home, shared by every bank. A line holds
// tokens and no money; what a call cost is worked out when the log is read,
// from a price table, so a changed price reprices the history.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	logFile    = "usage.jsonl"
	pricesFile = "prices.yaml"
)

// The purposes a call is recorded under.
const (
	Embed      = "embed"
	EmbedQuery = "embed_query"
	Extract    = "extract"
	Revise     = "revise"
	Merge      = "merge"
	Describe   = "describe"
	Enrich     = "enrich"
)

// Tokens is what a provider reports of one call: the tokens it read and
// wrote, over how many served requests. A provider that reports nothing — a
// local model — returns the zero value.
type Tokens struct {
	In, Out, Requests int
}

func (t *Tokens) Add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.Requests += o.Requests
}

// Record is one line of the log. Model is provider:model.
type Record struct {
	Time     time.Time `json:"time"`
	Bank     string    `json:"bank"`
	Purpose  string    `json:"purpose"`
	Model    string    `json:"model"`
	In       int       `json:"in"`
	Out      int       `json:"out,omitempty"`
	Requests int       `json:"requests"`
}

var mu sync.Mutex

// Append writes the call to the log under home. A call that reported no tokens
// is not written. The line goes out in one write to a file opened for append,
// so the UI server and an index run may both record.
func Append(home, bank, purpose, model string, t Tokens) error {
	if t.In == 0 && t.Out == 0 {
		return nil
	}
	line, err := json.Marshal(Record{Time: time.Now().UTC().Truncate(time.Second), Bank: bank, Purpose: purpose, Model: model, In: t.In, Out: t.Out, Requests: t.Requests})
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(home, logFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// Read is the whole log, oldest first. No log yet is no records.
func Read(home string) ([]Record, error) {
	raw, err := os.ReadFile(filepath.Join(home, logFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", logFile, n, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Price is dollars per million tokens.
type Price struct {
	In  float64 `yaml:"in" json:"in"`
	Out float64 `yaml:"out,omitempty" json:"out,omitempty"`
}

// defaults are list prices as published when this was written; they go stale,
// and `muninn usage price` states the current one. The same model costs the
// same through the Gemini API and through Vertex AI's global location; the
// tenth that Vertex adds in a regional location is not counted, since a record
// does not say where the call went.
var defaults = func() map[string]Price {
	gemini := map[string]Price{
		"gemini-2.5-flash-lite": {0.10, 0.40},
		"gemini-2.5-flash":      {0.30, 2.50},
		"gemini-3.1-flash-lite": {0.25, 1.50},
		"gemini-3.5-flash-lite": {0.30, 2.50},
		"gemini-3.5-flash":      {1.50, 9.00},
		// Introductory prices, through 2026-12-31; 1.50 and 7.50 after.
		"gemini-3.6-flash": {0.75, 3.75},
		"gemini-3.7-flash": {0.75, 3.75},
		"gemini-3.8-flash": {0.75, 3.75},
	}
	out := map[string]Price{
		"vertex:gemini-embedding-001":   {In: 0.15},
		"openai:text-embedding-3-small": {In: 0.02},
		"openai:text-embedding-3-large": {In: 0.13},
		"anthropic:claude-haiku-4-5":    {1, 5},
	}
	for model, p := range gemini {
		out["gemini:"+model] = p
		out["vertex:"+model] = p
	}
	return out
}()

func readOverrides(home string) (map[string]Price, error) {
	raw, err := os.ReadFile(filepath.Join(home, pricesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Price{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Price{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", pricesFile, err)
	}
	return out, nil
}

// Prices is the table the log is priced with: the defaults, under what
// `muninn usage price` has stated.
func Prices(home string) (map[string]Price, error) {
	over, err := readOverrides(home)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Price, len(defaults)+len(over))
	for m, p := range defaults {
		out[m] = p
	}
	for m, p := range over {
		out[m] = p
	}
	return out, nil
}

// SetPrice states the price of a model, over the default if there is one.
func SetPrice(home, model string, p Price) error {
	over, err := readOverrides(home)
	if err != nil {
		return err
	}
	over[model] = p
	raw, err := yaml.Marshal(over)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, pricesFile), raw, 0o644)
}

// Line is the records that share a key, summed.
type Line struct {
	Key      string  `json:"key"`
	Calls    int     `json:"calls"`
	Requests int     `json:"requests"`
	In       int     `json:"in"`
	Out      int     `json:"out"`
	Cost     float64 `json:"cost"`
	// Unpriced is how many of the calls were to a model with no price; Cost
	// leaves them out.
	Unpriced int `json:"unpriced"`
}

func (l *Line) add(r Record, prices map[string]Price) {
	l.Calls++
	l.Requests += r.Requests
	l.In += r.In
	l.Out += r.Out
	if p, ok := prices[r.Model]; ok {
		l.Cost += (float64(r.In)*p.In + float64(r.Out)*p.Out) / 1e6
	} else {
		l.Unpriced++
	}
}

// Groupings of a summary.
var By = map[string]func(Record) string{
	"bank":    func(r Record) string { return r.Bank },
	"model":   func(r Record) string { return r.Model },
	"purpose": func(r Record) string { return r.Purpose },
	"day":     func(r Record) string { return r.Time.Local().Format("2006-01-02") },
}

// Filter keeps the records of a bank ("" is every bank) since a time.
func Filter(recs []Record, bank string, since time.Time) []Record {
	var out []Record
	for _, r := range recs {
		if (bank == "" || r.Bank == bank) && !r.Time.Before(since) {
			out = append(out, r)
		}
	}
	return out
}

// Summarize sums the records by key, the dearest line first — by day, in
// order — and returns the total beside them.
func Summarize(recs []Record, prices map[string]Price, by string) ([]Line, Line, error) {
	key, ok := By[by]
	if !ok {
		return nil, Line{}, fmt.Errorf("usage is summed by bank, model, purpose or day, not by %q", by)
	}
	total := Line{Key: "total"}
	lines := map[string]*Line{}
	for _, r := range recs {
		k := key(r)
		if lines[k] == nil {
			lines[k] = &Line{Key: k}
		}
		lines[k].add(r, prices)
		total.add(r, prices)
	}
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if by != "day" && out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Key < out[j].Key
	})
	return out, total, nil
}
