package semantic

import (
	"fmt"
	"math"
	"sort"

	"github.com/kgatilin/muninn/internal/bank"
)

const (
	Accept        = "accept"
	Reject        = "reject"
	NotApplicable = "not_applicable"
)

// Snapshot is one side of a comparison: the region as a view, and the global
// counters, both as they stood before the patch or as they stand after it.
type Snapshot struct {
	View   *GraphView
	Counts Counts
}

// Result is a check's evidence and its decision. A band check reports a loss,
// the distance from its target, on both sides; the patch is accepted when the
// loss did not grow by more than MaxRegression. The target is a reference: a
// region outside it still accepts what improves it or leaves it alone.
type Result struct {
	Check         string             `json:"check"`
	Version       string             `json:"version"`
	Decision      string             `json:"decision"`
	Before        Measurement        `json:"before"`
	After         Measurement        `json:"after"`
	LossBefore    float64            `json:"loss_before"`
	LossAfter     float64            `json:"loss_after"`
	Delta         float64            `json:"delta"`
	MaxRegression float64            `json:"max_regression"`
	Reason        string             `json:"reason,omitempty"`
	Params        map[string]float64 `json:"params,omitempty"`
}

// Check interprets a before and an after. Metrics above report values and
// applicability and hold no opinion; the opinion is here.
type Check interface {
	Kind() string
	Version() string
	Evaluate(before, after Snapshot) Result
}

// BuildChecks are the bank's enabled checks, in kind order.
func BuildChecks(s bank.Semantic) ([]Check, error) {
	var out []Check
	resolved := s.Resolved()
	kinds := make([]string, 0, len(resolved.Checks))
	for k := range resolved.Checks {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		c := resolved.Checks[kind]
		if c.Enabled == nil || !*c.Enabled {
			continue
		}
		mk, ok := checkMakers[kind]
		if !ok {
			return nil, fmt.Errorf("check %q is in the bank's settings and not in the engine", kind)
		}
		out = append(out, mk(c.Params))
	}
	return out, nil
}

var checkMakers = map[string]func(map[string]float64) Check{
	"scale_free":      func(p map[string]float64) Check { return scaleFreeCheck{p} },
	"fractal":         func(p map[string]float64) Check { return fractalCheck{p} },
	"entity_budget":   func(p map[string]float64) Check { return budgetCheck{p} },
	"singleton_share": func(p map[string]float64) Check { return singletonCheck{p} },
}

// band is how far v lies outside [low, high].
func band(v, low, high float64) float64 { return math.Max(0, math.Max(low-v, v-high)) }

// decide fills in the loss comparison. Either side without an estimate makes
// the check not applicable, and that does not block.
func decide(r Result, lossOf func(Measurement) float64) Result {
	r.MaxRegression = r.Params["max_regression"]
	if r.Before.Status != StatusOK || r.After.Status != StatusOK {
		r.Decision = NotApplicable
		r.Reason = fmt.Sprintf("before %s, after %s", r.Before.Status, r.After.Status)
		return r
	}
	r.LossBefore, r.LossAfter = lossOf(r.Before), lossOf(r.After)
	r.Delta = r.LossAfter - r.LossBefore
	r.Decision = Accept
	// The tolerance absorbs the last bits of two sums taken in different order.
	if r.Delta > r.MaxRegression+1e-12 {
		r.Decision = Reject
	}
	return r
}

type scaleFreeCheck struct{ p map[string]float64 }

func (scaleFreeCheck) Kind() string    { return "scale_free" }
func (scaleFreeCheck) Version() string { return ScaleFreeEstimator }

func (c scaleFreeCheck) loss(m Measurement) float64 {
	return band(m.Value, c.p["low"], c.p["high"]) +
		c.p["ks_weight"]*math.Max(0, m.Details["ks"]-c.p["ks"]) +
		c.p["rln_weight"]*math.Max(0, math.Abs(m.Details["r_ln"])-c.p["rln"])
}

func (c scaleFreeCheck) Evaluate(before, after Snapshot) Result {
	r := Result{Check: c.Kind(), Version: c.Version(), Params: c.p}
	minNodes, minTail := int(c.p["min_nodes"]), int(c.p["min_tail"])
	r.Before = ScaleFree(before.View, minNodes, minTail, 0)
	r.After = ScaleFree(after.View, minNodes, minTail, int(r.Before.Details["k_min"]))
	r = decide(r, c.loss)
	if r.Decision == Reject {
		r.Reason = fmt.Sprintf("γ %.3f → %.3f, KS %.3f → %.3f, R_ln %.2f → %.2f against γ in [%g, %g]",
			r.Before.Value, r.After.Value, r.Before.Details["ks"], r.After.Details["ks"],
			r.Before.Details["r_ln"], r.After.Details["r_ln"], c.p["low"], c.p["high"])
	}
	return r
}

type fractalCheck struct{ p map[string]float64 }

func (fractalCheck) Kind() string    { return "fractal" }
func (fractalCheck) Version() string { return FractalEstimator }

func (c fractalCheck) Evaluate(before, after Snapshot) Result {
	r := Result{Check: c.Kind(), Version: c.Version(), Params: c.p}
	minNodes, centres, radius := int(c.p["min_nodes"]), int(c.p["centres"]), int(c.p["radius"])
	r.Before = Fractal(before.View, minNodes, centres, radius)
	r.After = Fractal(after.View, minNodes, centres, radius)
	r = decide(r, func(m Measurement) float64 { return band(m.Value, c.p["low"], c.p["high"]) })
	switch {
	case r.Decision == Reject:
		r.Reason = fmt.Sprintf("d_B %.3f → %.3f against [%g, %g]", r.Before.Value, r.After.Value, c.p["low"], c.p["high"])
	case c.p["reject_collapse"] != 0 && r.Before.Status == StatusOK && r.After.Status == StatusSaturated:
		// The one undefined estimate that blocks: a region that had a dimension
		// and after the patch is within two hops of itself. Letting it through
		// as not applicable would wave in exactly the hub the check is for.
		r.Decision = Reject
		r.Reason = fmt.Sprintf("d_B was %.3f and the patch leaves the region saturated: the balls reach nine tenths of it within %d hops", r.Before.Value, int(r.After.Details["radii"])+1)
	}
	return r
}

// counted wraps a number that is always defined.
func counted(metric string, v float64, details map[string]float64) Measurement {
	return Measurement{Metric: metric, Status: StatusOK, Value: v, Details: details}
}

type budgetCheck struct{ p map[string]float64 }

func (budgetCheck) Kind() string    { return "entity_budget" }
func (budgetCheck) Version() string { return "entity-budget/1" }

func (c budgetCheck) Evaluate(before, after Snapshot) Result {
	r := Result{Check: c.Kind(), Version: c.Version(), Params: c.p}
	measure := func(n Counts) Measurement {
		return counted("entities", float64(n.Entities), map[string]float64{"texts": float64(n.Texts)})
	}
	r.Before, r.After = measure(before.Counts), measure(after.Counts)
	r = decide(r, func(m Measurement) float64 {
		return math.Max(0, m.Value-(c.p["allowance"]+c.p["ratio"]*m.Details["texts"]))
	})
	if r.Decision == Reject {
		r.Reason = fmt.Sprintf("%d entities over %d text nodes; the budget is %g + %g per text node",
			after.Counts.Entities, after.Counts.Texts, c.p["allowance"], c.p["ratio"])
	}
	return r
}

type singletonCheck struct{ p map[string]float64 }

func (singletonCheck) Kind() string    { return "singleton_share" }
func (singletonCheck) Version() string { return "singleton-share/1" }

func (c singletonCheck) Evaluate(before, after Snapshot) Result {
	r := Result{Check: c.Kind(), Version: c.Version(), Params: c.p}
	measure := func(n Counts) Measurement {
		if float64(n.Entities) < c.p["min_entities"] || n.Entities == 0 {
			return Measurement{Metric: "singleton_share", Status: StatusTooSmall}
		}
		return counted("singleton_share", float64(n.Singletons)/float64(n.Entities), map[string]float64{"entities": float64(n.Entities)})
	}
	r.Before, r.After = measure(before.Counts), measure(after.Counts)
	r = decide(r, func(m Measurement) float64 { return math.Max(0, m.Value-c.p["ceiling"]) })
	if r.Decision == Reject {
		r.Reason = fmt.Sprintf("share of entities with one mention %.3f → %.3f over the ceiling %g", r.Before.Value, r.After.Value, c.p["ceiling"])
	}
	return r
}
