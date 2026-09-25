package semantic

import (
	"testing"

	"github.com/kgatilin/muninn/internal/bank"
)

func TestDiagnoseAndCheckBank(t *testing.T) {
	const texts = 300
	state := newTexts(texts)
	g := &Gate{Layer: NewLayer(state), Hops: 2, RegionCap: 5000}
	grow(t, g, texts, 3, 3)

	d := Diagnose(state, bank.Semantic{}, 3)
	if d.Nodes["document"] != texts || d.Nodes["directory"] != 1 || d.Edges["contains"] != texts || d.Edges[EdgeMentions] != 3*texts {
		t.Errorf("counts: %v %v", d.Nodes, d.Edges)
	}
	if d.Components.Count != 1 || d.Components.LargestShare != 1 {
		t.Errorf("components: %+v", d.Components)
	}
	if hubs := d.Degrees["directory"].Hubs; len(hubs) != 1 || hubs[0].ID != "/t" || hubs[0].Degree != texts {
		t.Errorf("the directory's hubs: %+v", hubs)
	}
	if hubs := d.Degrees[KindEntity].Hubs; len(hubs) != 3 || hubs[0].Degree < hubs[1].Degree || hubs[1].Degree < hubs[2].Degree {
		t.Errorf("the entities' hubs: %+v", hubs)
	}
	// The directory everything hangs off is in the full graph and not in the
	// semantic one, and only the full graph is within two hops of itself.
	if d.Full.Nodes != d.Semantic.Nodes+1 || d.Semantic.Fractal.Status != StatusOK || d.Full.Fractal.Status != StatusSaturated {
		t.Errorf("full %+v, semantic %+v", d.Full, d.Semantic)
	}

	findings := CheckBank(state, bank.Semantic{}, 3)
	if len(findings) != 4 {
		t.Fatalf("findings: %+v", findings)
	}
	for _, f := range findings {
		t.Logf("%s %s %s %s %.3f %s", f.Graph, f.Check, f.Verdict, f.Measure.Status, f.Measure.Value, f.Note)
		if f.Graph == "full" && f.Check == "fractal" {
			if f.Verdict != "miss" || len(f.Blame) == 0 || f.Blame[0].ID != "/t" {
				t.Errorf("the full graph's fractal finding: %+v", f)
			}
		}
	}
}
