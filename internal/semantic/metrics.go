package semantic

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

// Status is a measurement's applicability. Only StatusOK carries a value.
const (
	StatusOK = "ok"
	// StatusTooSmall: too few nodes, or too short a tail, for an estimate.
	StatusTooSmall = "too_small"
	// StatusNoFit: enough nodes and no fit — a degenerate tail, a flat curve.
	StatusNoFit = "no_fit"
	// StatusSaturated: the balls reach most of the view before three radii, so
	// there is no growth left to take a slope of. A hairball measures like this.
	StatusSaturated = "saturated"
)

// Measurement is what a metric reports: a value when it applies, and why not
// when it does not. It carries no judgement; checks read it.
type Measurement struct {
	Metric  string             `json:"metric"`
	Status  string             `json:"status"`
	Value   float64            `json:"value"`
	Details map[string]float64 `json:"details,omitempty"`
}

// Estimator versions, recorded with every decision. They change when the same
// graph would measure differently.
const (
	ScaleFreeEstimator = "power-law-csn/1"
	FractalEstimator   = "ball-growth/1"
)

// ScaleFree fits a discrete power law to the positive degrees of the view
// (Clauset, Shalizi, Newman 2009): for every candidate k_min the exponent is
// the maximum-likelihood one for p(k) = k^-γ / ζ(γ, k_min), and the k_min kept
// is the one whose fit has the smallest Kolmogorov–Smirnov distance to the
// tail. Value is γ, searched in [1.05, 8]. Details: k_min, tail, ks, and r_ln — the log-likelihood
// ratio of the power law against a discretized lognormal on the same tail,
// divided by its standard deviation (Vuong); positive favours the power law.
//
// fixKMin, when positive, skips the search: the after-measurement of a patch
// reuses the baseline's cutoff, so the two fits are over the same population
// and a cutoff that hops between two near-equal minima does not read as a
// change in γ.
func ScaleFree(v *GraphView, minNodes, minTail, fixKMin int) Measurement {
	m := Measurement{Metric: "scale_free", Status: StatusTooSmall}
	hist := map[int]int{}
	n := 0
	for _, id := range v.Nodes {
		if d := v.Degree[id]; d > 0 {
			hist[d]++
			n++
		}
	}
	if n < minNodes {
		return m
	}
	ks := make([]int, 0, len(hist))
	for k := range hist {
		ks = append(ks, k)
	}
	sort.Ints(ks)

	best := struct {
		kmin, tail int
		gamma, ks  float64
	}{ks: math.Inf(1)}
	for i, kmin := range ks {
		if fixKMin > 0 && kmin != fixKMin {
			continue
		}
		tail, sumLog := 0, 0.0
		for _, k := range ks[i:] {
			tail += hist[k]
			sumLog += float64(hist[k]) * math.Log(float64(k))
		}
		// A tail needs enough nodes and at least two degrees to have a slope.
		if tail < minTail || len(ks)-i < 2 {
			continue
		}
		gamma := fitGamma(float64(kmin), float64(tail), sumLog)
		if d := ksDistance(ks[i:], hist, tail, gamma); d < best.ks {
			best.kmin, best.tail, best.gamma, best.ks = kmin, tail, gamma, d
		}
	}
	if best.kmin == 0 {
		if fixKMin > 0 {
			return ScaleFree(v, minNodes, minTail, 0)
		}
		if len(ks) < 2 {
			m.Status = StatusNoFit
		}
		return m
	}
	i := sort.SearchInts(ks, best.kmin)
	m.Status, m.Value = StatusOK, best.gamma
	m.Details = map[string]float64{
		"k_min": float64(best.kmin), "tail": float64(best.tail), "ks": best.ks,
		"r_ln": lognormalRatio(ks[i:], hist, best.tail, best.gamma),
	}
	// An exponent at the edge of the search is the edge and not the maximum:
	// degrees bunched around one value, as a hairball's are, want a steeper law
	// than any the search allows. It is reported as it is, far outside any
	// band, and marked.
	if best.gamma <= gammaLow+1e-5 || best.gamma >= gammaHigh-1e-5 {
		m.Details["clamped"] = 1
	}
	return m
}

const gammaLow, gammaHigh = 1.05, 8.0

// fitGamma maximizes -n ln ζ(γ, k_min) - γ Σ ln k by golden section; the
// likelihood is unimodal in γ.
func fitGamma(kmin, n, sumLog float64) float64 {
	ll := func(g float64) float64 { return -n*math.Log(hurwitz(g, kmin)) - g*sumLog }
	const phi = 0.6180339887498949
	a, b := gammaLow, gammaHigh
	c, d := b-phi*(b-a), a+phi*(b-a)
	fc, fd := ll(c), ll(d)
	for b-a > 1e-7 {
		if fc > fd {
			b, d, fd = d, c, fc
			c = b - phi*(b-a)
			fc = ll(c)
		} else {
			a, c, fc = c, d, fd
			d = a + phi*(b-a)
			fd = ll(d)
		}
	}
	return (a + b) / 2
}

// hurwitz is ζ(s, q) = Σ (q+k)^-s: twenty terms, then Euler–Maclaurin for the
// rest.
func hurwitz(s, q float64) float64 {
	const terms = 20
	sum := 0.0
	for k := 0; k < terms; k++ {
		sum += math.Pow(q+float64(k), -s)
	}
	x := q + terms
	sum += math.Pow(x, 1-s)/(s-1) + math.Pow(x, -s)/2
	sum += s * math.Pow(x, -s-1) / 12
	sum -= s * (s + 1) * (s + 2) * math.Pow(x, -s-3) / 720
	return sum
}

// ksDistance is the largest gap between the tail's empirical CDF and the
// fitted one, P(K ≤ k) = 1 - ζ(γ, k+1)/ζ(γ, k_min).
func ksDistance(tail []int, hist map[int]int, n int, gamma float64) float64 {
	z := hurwitz(gamma, float64(tail[0]))
	worst, seen := 0.0, 0
	for _, k := range tail {
		model := 1 - hurwitz(gamma, float64(k))/z // P(K < k)
		worst = math.Max(worst, math.Abs(float64(seen)/float64(n)-model))
		seen += hist[k]
		model = 1 - hurwitz(gamma, float64(k+1))/z
		worst = math.Max(worst, math.Abs(float64(seen)/float64(n)-model))
	}
	return worst
}

// lognormalRatio is R/(σ√n) for the power law against a lognormal discretized
// by rounding, p(k) ∝ Φ((ln(k+½)-μ)/σ) - Φ((ln(k-½)-μ)/σ) for k ≥ k_min, its μ
// and σ fitted by maximum likelihood on the same tail.
func lognormalRatio(tail []int, hist map[int]int, n int, gamma float64) float64 {
	kmin := float64(tail[0])
	// The survival function and not the CDF: the difference of two values near
	// one loses the far tail, where a degree distribution has its hubs.
	above := func(x, mu, sigma float64) float64 { return 0.5 * math.Erfc((math.Log(x)-mu)/(sigma*math.Sqrt2)) }
	logp := func(k, mu, sigma float64) float64 {
		p := above(k-0.5, mu, sigma) - above(k+0.5, mu, sigma)
		norm := above(kmin-0.5, mu, sigma)
		if p <= 0 || norm <= 0 {
			return -745 // ln of the smallest positive float: a point the model cannot produce
		}
		return math.Log(p / norm)
	}
	var mean, sq float64
	for _, k := range tail {
		mean += float64(hist[k]) * math.Log(float64(k))
	}
	mean /= float64(n)
	for _, k := range tail {
		d := math.Log(float64(k)) - mean
		sq += float64(hist[k]) * d * d
	}
	start := [2]float64{mean, math.Max(math.Sqrt(sq/float64(n)), 0.1)}
	mu, sigma := nelderMead(func(x [2]float64) float64 {
		if x[1] < 1e-3 {
			return math.Inf(1)
		}
		var ll float64
		for _, k := range tail {
			ll += float64(hist[k]) * logp(float64(k), x[0], x[1])
		}
		return -ll
	}, start)

	lz := math.Log(hurwitz(gamma, kmin))
	var r, r2 float64
	for _, k := range tail {
		d := -gamma*math.Log(float64(k)) - lz - logp(float64(k), mu, sigma)
		r += float64(hist[k]) * d
		r2 += float64(hist[k]) * d * d
	}
	nn := float64(n)
	variance := r2/nn - (r/nn)*(r/nn)
	if variance <= 1e-18 {
		return 0
	}
	return r / math.Sqrt(variance*nn)
}

// nelderMead minimizes a function of two variables from a start; a fixed
// number of steps, no randomness.
func nelderMead(f func([2]float64) float64, start [2]float64) (float64, float64) {
	type vertex struct {
		x [2]float64
		f float64
	}
	mk := func(x [2]float64) vertex { return vertex{x, f(x)} }
	s := []vertex{mk(start), mk([2]float64{start[0] + 0.5, start[1]}), mk([2]float64{start[0], start[1] * 1.5})}
	along := func(a, b [2]float64, t float64) [2]float64 {
		return [2]float64{a[0] + t*(b[0]-a[0]), a[1] + t*(b[1]-a[1])}
	}
	for step := 0; step < 200; step++ {
		sort.SliceStable(s, func(i, j int) bool { return s[i].f < s[j].f })
		if math.Abs(s[2].f-s[0].f) < 1e-10 {
			break
		}
		centre := along(s[0].x, s[1].x, 0.5)
		reflected := mk(along(s[2].x, centre, 2))
		switch {
		case reflected.f < s[0].f:
			if expanded := mk(along(s[2].x, centre, 3)); expanded.f < reflected.f {
				s[2] = expanded
			} else {
				s[2] = reflected
			}
		case reflected.f < s[1].f:
			s[2] = reflected
		default:
			if contracted := mk(along(s[2].x, centre, 0.5)); contracted.f < s[2].f {
				s[2] = contracted
			} else {
				s[1] = mk(along(s[0].x, s[1].x, 0.5))
				s[2] = mk(along(s[0].x, s[2].x, 0.5))
			}
		}
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].f < s[j].f })
	return s[0].x[0], s[0].x[1]
}

// Fractal estimates a dimension by ball growth: from a fixed sample of
// centres, N(r) is the mean number of nodes within r hops in the induced
// graph, and the value is the least-squares slope of ln N(r) against ln r over
// the radii before the balls saturate — mean N(r) under nine tenths of the
// view and still growing. It is a ball-growth approximation and not a
// box-covering dimension. Details: radii, centres, and n1..n<radius>.
func Fractal(v *GraphView, minNodes, centres, maxRadius int) Measurement {
	m := Measurement{Metric: "fractal", Status: StatusTooSmall}
	if len(v.Nodes) < minNodes {
		return m
	}
	picked := sampleCentres(v, centres)
	if len(picked) == 0 {
		return m
	}
	sums := make([]float64, maxRadius+1)
	dist := map[string]int{}
	for _, c := range picked {
		clear(dist)
		dist[c] = 0
		queue := []string{c}
		within := make([]int, maxRadius+1)
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			within[dist[u]]++
			if dist[u] == maxRadius {
				continue
			}
			for _, w := range v.Adj[u] {
				if _, ok := dist[w]; !ok {
					dist[w] = dist[u] + 1
					queue = append(queue, w)
				}
			}
		}
		total := 0
		for r, c := range within {
			total += c
			sums[r] += float64(total)
		}
	}
	m.Details = map[string]float64{"centres": float64(len(picked))}
	var xs, ys []float64
	saturated := false
	for r := 1; r <= maxRadius; r++ {
		mean := sums[r] / float64(len(picked))
		m.Details[fmt.Sprintf("n%d", r)] = mean
		if mean >= 0.9*float64(len(v.Nodes)) {
			saturated = true
			break
		}
		// Balls that stopped growing under the ceiling ran out of component,
		// not out of view: that is a small graph, not a hairball.
		if mean <= sums[r-1]/float64(len(picked)) {
			break
		}
		xs, ys = append(xs, math.Log(float64(r))), append(ys, math.Log(mean))
	}
	m.Details["radii"] = float64(len(xs))
	if len(xs) < 3 {
		if saturated {
			m.Status = StatusSaturated
		}
		return m
	}
	slope, ok := leastSquares(xs, ys)
	if !ok {
		m.Status = StatusNoFit
		return m
	}
	m.Status, m.Value = StatusOK, slope
	return m
}

// sampleCentres takes the anchors that have an edge in the view, in the order
// of a hash of their ids: the same nodes every time, spread over the view, and
// the same ones before and after a patch.
func sampleCentres(v *GraphView, k int) []string {
	type hashed struct {
		id string
		h  uint64
	}
	var all []hashed
	for _, id := range v.Anchors {
		if len(v.Adj[id]) == 0 {
			continue
		}
		h := fnv.New64a()
		h.Write([]byte("muninn-centres\x00" + id))
		all = append(all, hashed{id, h.Sum64()})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].h != all[j].h {
			return all[i].h < all[j].h
		}
		return all[i].id < all[j].id
	})
	out := make([]string, 0, k)
	for _, a := range all[:min(k, len(all))] {
		out = append(out, a.id)
	}
	return out
}

func leastSquares(xs, ys []float64) (slope float64, ok bool) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx, sy, sxx, sxy = sx+xs[i], sy+ys[i], sxx+xs[i]*xs[i], sxy+xs[i]*ys[i]
	}
	den := n*sxx - sx*sx
	if den <= 0 {
		return 0, false
	}
	slope = (n*sxy - sx*sy) / den
	return slope, !math.IsNaN(slope) && !math.IsInf(slope, 0)
}
