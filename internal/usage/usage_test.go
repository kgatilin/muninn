package usage

import (
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAppendReadAndSummarize(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home") // made by the first line
	chat, embedder := "vertex:gemini-3.5-flash-lite", "vertex:gemini-embedding-001"
	for _, c := range []struct {
		bank, purpose, model string
		t                    Tokens
	}{
		{"notes", Embed, embedder, Tokens{In: 2_000_000, Requests: 40}},
		{"notes", Extract, chat, Tokens{In: 1_000_000, Out: 100_000, Requests: 1}},
		{"notes", Revise, chat, Tokens{In: 500_000, Out: 0, Requests: 1}},
		{"code", Extract, "vertex:some-new-model", Tokens{In: 10, Out: 10, Requests: 1}},
		{"code", Embed, "ollama:local", Tokens{}}, // a free call is not a line
	} {
		if err := Append(home, c.bank, c.purpose, c.model, c.t); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Read(home)
	if err != nil || len(recs) != 4 {
		t.Fatalf("%d records: %v", len(recs), err)
	}
	prices, err := Prices(home)
	if err != nil {
		t.Fatal(err)
	}
	lines, total, err := Summarize(Filter(recs, "notes", time.Time{}), prices, "purpose")
	if err != nil || len(lines) != 3 {
		t.Fatalf("%v %v", lines, err)
	}
	// extract 0.30+0.25, embed 0.30, revise 0.15: the dearest first.
	if lines[0].Key != Extract || math.Abs(lines[0].Cost-0.55) > 1e-9 || lines[1].Key != Embed || lines[2].Key != Revise {
		t.Fatalf("lines %+v", lines)
	}
	if total.Calls != 3 || total.Requests != 42 || total.In != 3_500_000 || math.Abs(total.Cost-1.0) > 1e-9 || total.Unpriced != 0 {
		t.Fatalf("total %+v", total)
	}

	// A model with no price is counted and left out of the cost, until one is stated.
	_, all, _ := Summarize(recs, prices, "bank")
	if all.Calls != 4 || all.Unpriced != 1 || math.Abs(all.Cost-1.0) > 1e-9 {
		t.Fatalf("all %+v", all)
	}
	if err := SetPrice(home, "vertex:some-new-model", Price{In: 1e5, Out: 1e5}); err != nil {
		t.Fatal(err)
	}
	if err := SetPrice(home, embedder, Price{In: 0}); err != nil {
		t.Fatal(err)
	}
	prices, _ = Prices(home)
	if _, all, _ = Summarize(recs, prices, "bank"); all.Unpriced != 0 || math.Abs(all.Cost-(0.7+2)) > 1e-9 {
		t.Fatalf("repriced %+v", all)
	}

	if _, _, err := Summarize(recs, prices, "colour"); err == nil {
		t.Fatal("an unknown grouping")
	}
	if got := Filter(recs, "", time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("records from the future: %v", got)
	}
}

func TestNoLogIsNoRecords(t *testing.T) {
	if recs, err := Read(t.TempDir()); err != nil || recs != nil {
		t.Fatalf("%v %v", recs, err)
	}
}

func TestConcurrentAppendsKeepWholeLines(t *testing.T) {
	home := t.TempDir()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Append(home, "b", Embed, "m", Tokens{In: 1, Requests: 1}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	recs, err := Read(home)
	if err != nil || len(recs) != 50 {
		t.Fatalf("%d records: %v", len(recs), err)
	}
	if err := os.WriteFile(filepath.Join(home, logFile), []byte("{broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home); err == nil {
		t.Fatal("a broken line read without an error")
	}
}
