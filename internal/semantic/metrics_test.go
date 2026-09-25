package semantic

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// viewOf is a whole graph as a view, from undirected pairs.
func viewOf(pairs [][2]int) *GraphView {
	sets := map[string]map[string]bool{}
	for _, p := range pairs {
		a, b := fmt.Sprintf("n%05d", p[0]), fmt.Sprintf("n%05d", p[1])
		if a == b {
			continue
		}
		for _, q := range [2][2]string{{a, b}, {b, a}} {
			if sets[q[0]] == nil {
				sets[q[0]] = map[string]bool{}
			}
			sets[q[0]][q[1]] = true
		}
	}
	v := &GraphView{Degree: map[string]int{}, Adj: map[string][]string{}}
	for id, nbrs := range sets {
		v.Nodes = append(v.Nodes, id)
		v.Degree[id] = len(nbrs)
		for w := range nbrs {
			v.Adj[id] = append(v.Adj[id], w)
		}
		sort.Strings(v.Adj[id])
	}
	sort.Strings(v.Nodes)
	v.Anchors = v.Nodes
	return v
}

// preferential is Barabási–Albert growth: every new node attaches m edges to
// old ones picked in proportion to their degree.
func preferential(n, m int, seed int64) [][2]int {
	r := rand.New(rand.NewSource(seed))
	var pairs [][2]int
	var ends []int // every edge end once: a draw from it is degree-proportional
	for i := 0; i <= m; i++ {
		for j := 0; j < i; j++ {
			pairs = append(pairs, [2]int{i, j})
			ends = append(ends, i, j)
		}
	}
	for v := m + 1; v < n; v++ {
		picked := map[int]bool{}
		for len(picked) < m {
			picked[ends[r.Intn(len(ends))]] = true
		}
		targets := make([]int, 0, m)
		for t := range picked {
			targets = append(targets, t)
		}
		sort.Ints(targets)
		for _, t := range targets {
			pairs = append(pairs, [2]int{v, t})
			ends = append(ends, v, t)
		}
	}
	return pairs
}

func ring(n int) [][2]int {
	var pairs [][2]int
	for i := 0; i < n; i++ {
		pairs = append(pairs, [2]int{i, (i + 1) % n})
	}
	return pairs
}

func lattice(side int) [][2]int {
	var pairs [][2]int
	for x := 0; x < side; x++ {
		for y := 0; y < side; y++ {
			if x+1 < side {
				pairs = append(pairs, [2]int{x*side + y, (x+1)*side + y})
			}
			if y+1 < side {
				pairs = append(pairs, [2]int{x*side + y, x*side + y + 1})
			}
		}
	}
	return pairs
}

func star(n int) [][2]int {
	var pairs [][2]int
	for i := 1; i < n; i++ {
		pairs = append(pairs, [2]int{0, i})
	}
	return pairs
}

func random(n int, p float64, seed int64) [][2]int {
	r := rand.New(rand.NewSource(seed))
	var pairs [][2]int
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			if r.Float64() < p {
				pairs = append(pairs, [2]int{i, j})
			}
		}
	}
	return pairs
}

func TestHurwitzAgainstKnownValues(t *testing.T) {
	for _, c := range []struct{ s, q, want float64 }{
		{2, 1, math.Pi * math.Pi / 6},
		{4, 1, math.Pow(math.Pi, 4) / 90},
		{2, 2, math.Pi*math.Pi/6 - 1},
		{3, 1, 1.2020569031595942},
	} {
		if got := hurwitz(c.s, c.q); math.Abs(got-c.want) > 1e-10 {
			t.Errorf("ζ(%g, %g) = %.12f, want %.12f", c.s, c.q, got, c.want)
		}
	}
}

// A sample drawn from a known discrete power law comes back with its exponent.
func TestScaleFreeRecoversAKnownExponent(t *testing.T) {
	for _, gamma := range []float64{2.2, 2.5, 3.0} {
		r := rand.New(rand.NewSource(7))
		z := hurwitz(gamma, 1)
		v := &GraphView{Degree: map[string]int{}}
		for i := 0; i < 5000; i++ {
			u, k, acc := r.Float64(), 1, 0.0
			for ; k < 100000; k++ {
				if acc += math.Pow(float64(k), -gamma) / z; acc >= u {
					break
				}
			}
			id := fmt.Sprintf("n%05d", i)
			v.Nodes = append(v.Nodes, id)
			v.Degree[id] = k
		}
		m := ScaleFree(v, 50, 10, 0)
		if m.Status != StatusOK || math.Abs(m.Value-gamma) > 0.15 || m.Details["ks"] > 0.05 {
			t.Errorf("γ=%g: %+v", gamma, m)
		}
		t.Logf("drawn γ=%g: fitted %.3f, k_min %g, KS %.4f, R_ln %.2f", gamma, m.Value, m.Details["k_min"], m.Details["ks"], m.Details["r_ln"])
	}
}

func TestMetricsOnKnownGraphs(t *testing.T) {
	check := func(name string, m Measurement, status string, low, high float64) {
		t.Helper()
		t.Logf("%-22s %-10s %-9s %.3f %v", name, m.Metric, m.Status, m.Value, m.Details)
		if m.Status != status {
			t.Errorf("%s %s: status %s, want %s", name, m.Metric, m.Status, status)
		}
		if status == StatusOK && (m.Value < low || m.Value > high) {
			t.Errorf("%s %s = %.3f, want within [%g, %g]", name, m.Metric, m.Value, low, high)
		}
	}
	scale := func(v *GraphView) Measurement { return ScaleFree(v, 50, 10, 0) }
	fractal := func(v *GraphView) Measurement { return Fractal(v, 50, 64, 6) }

	// Preferential attachment has γ = 3 in the limit; at this size the fit
	// lands a little under it.
	for seed := int64(1); seed <= 3; seed++ {
		ba := viewOf(preferential(3000, 3, seed))
		m := scale(ba)
		check(fmt.Sprintf("preferential seed %d", seed), m, StatusOK, 2.5, 3.5)
		if m.Details["ks"] > 0.1 {
			t.Errorf("preferential attachment: KS %.3f", m.Details["ks"])
		}
		check(fmt.Sprintf("preferential seed %d", seed), fractal(ba), StatusOK, 2.5, 6)
	}

	// A ring is one-dimensional and a square lattice two; ball growth over six
	// radii reads a little under both, since N(r) = 2r+1 and 2r²+2r+1 carry
	// lower-order terms.
	check("ring", fractal(viewOf(ring(600))), StatusOK, 0.7, 1.1)
	check("lattice", fractal(viewOf(lattice(40))), StatusOK, 1.5, 2.1)
	// Every node of either has the same degree, or nearly: no tail to fit.
	check("ring", scale(viewOf(ring(600))), StatusNoFit, 0, 0)

	// A star has no dimension — one hop from the hub is everything — and a
	// dense random graph is the same hairball without the hub.
	check("star", fractal(viewOf(star(500))), StatusSaturated, 0, 0)
	check("hairball", fractal(viewOf(random(300, 0.2, 1))), StatusSaturated, 0, 0)
	// Its degrees are bunched around one value, which a power law fits only
	// with an exponent far above the band.
	if m := scale(viewOf(random(300, 0.2, 1))); m.Status == StatusOK && band(m.Value, 2, 3.5) == 0 {
		t.Errorf("hairball γ = %.3f sits inside the band", m.Value)
	} else {
		t.Logf("hairball               scale_free %s %.3f %v", m.Status, m.Value, m.Details)
	}
	if m := scale(viewOf(star(500))); m.Status == StatusOK && band(m.Value, 2, 3.5) == 0 {
		t.Errorf("star γ = %.3f sits inside the band", m.Value)
	} else {
		t.Logf("star                   scale_free %s %.3f %v", m.Status, m.Value, m.Details)
	}

	tiny := viewOf(ring(12))
	check("tiny", scale(tiny), StatusTooSmall, 0, 0)
	check("tiny", fractal(tiny), StatusTooSmall, 0, 0)
	check("empty", scale(&GraphView{}), StatusTooSmall, 0, 0)
	check("empty", fractal(&GraphView{}), StatusTooSmall, 0, 0)
}

func TestMetricsAreDeterministic(t *testing.T) {
	a, b := viewOf(preferential(800, 2, 5)), viewOf(preferential(800, 2, 5))
	if x, y := ScaleFree(a, 50, 10, 0), ScaleFree(b, 50, 10, 0); fmt.Sprint(x) != fmt.Sprint(y) {
		t.Errorf("scale_free differs between two runs: %v, %v", x, y)
	}
	if x, y := Fractal(a, 50, 64, 6), Fractal(b, 50, 64, 6); fmt.Sprint(x) != fmt.Sprint(y) {
		t.Errorf("fractal differs between two runs: %v, %v", x, y)
	}
}
