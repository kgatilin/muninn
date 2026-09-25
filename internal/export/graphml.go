// Package export writes a bank's graph for other tools to read.
package export

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/kgatilin/muninn/internal/index"
)

type Options struct {
	// Kinds and EdgeKinds, when not empty, are what is written.
	Kinds, EdgeKinds []string
}

type Result struct {
	Nodes, Edges int
	// Dangling is the edges left out because an end is not a node of the bank.
	// An edge to a node of a kind not asked for is left out and not counted.
	Dangling int
}

type key struct{ id, scope, name, typ string }

// The keys are archmotif's dialect, which is also what Gephi shows: generated
// XML ids, the node's own id in the `id` attribute, `label` and `kind` on a
// node, `kind` on an edge. archmotif's readers refuse an edge whose end is not
// in the file, which is why dangling edges are not written.
var keys = []key{
	{"n_id", "node", "id", "string"},
	{"n_label", "node", "label", "string"},
	{"n_kind", "node", "kind", "string"},
	{"n_connector", "node", "connector", "string"},
	{"n_chunks", "node", "chunks", "int"},
	{"n_degree", "node", "degree", "int"},
	{"n_at", "node", "at", "string"},
	{"e_kind", "edge", "kind", "string"},
	{"e_weight", "edge", "weight", "double"},
	{"e_connector", "edge", "connector", "string"},
}

// GraphML writes the graph directed, nodes by id and edges by (from, to,
// kind), so two exports of one snapshot are the same bytes. An edge's weight
// is as stored: 0 when the connector stated none. degree counts the edges
// written.
func GraphML(w io.Writer, state *index.State, o Options) (Result, error) {
	var res Result
	kinds, edgeKinds := set(o.Kinds), set(o.EdgeKinds)

	xmlID := map[string]string{}
	var ids []string
	for id, n := range state.Nodes {
		if len(kinds) == 0 || kinds[n.Kind] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		xmlID[id] = "n" + strconv.Itoa(i)
	}

	var edges []*index.Edge
	degree := map[string]int{}
	for _, e := range state.Edges {
		if len(edgeKinds) > 0 && !edgeKinds[e.Kind] {
			continue
		}
		if state.Nodes[e.From] == nil || state.Nodes[e.To] == nil {
			res.Dangling++
			continue
		}
		if xmlID[e.From] == "" || xmlID[e.To] == "" {
			continue
		}
		edges = append(edges, e)
		degree[e.From]++
		if e.To != e.From {
			degree[e.To]++
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
	res.Nodes, res.Edges = len(ids), len(edges)

	bw := bufio.NewWriter(w)
	fmt.Fprint(bw, xml.Header, `<graphml xmlns="http://graphml.graphdrawing.org/xmlns">`, "\n")
	for _, k := range keys {
		fmt.Fprintf(bw, "  <key id=%q for=%q attr.name=%q attr.type=%q/>\n", k.id, k.scope, k.name, k.typ)
	}
	fmt.Fprint(bw, `  <graph id="G" edgedefault="directed">`, "\n")
	for _, id := range ids {
		n := state.Nodes[id]
		fmt.Fprintf(bw, "    <node id=%q>\n", xmlID[id])
		data(bw, "n_id", id)
		data(bw, "n_label", n.Label())
		data(bw, "n_kind", n.Kind)
		data(bw, "n_connector", n.Connector)
		data(bw, "n_chunks", strconv.Itoa(len(n.Chunks)))
		data(bw, "n_degree", strconv.Itoa(degree[id]))
		if !n.At.IsZero() {
			data(bw, "n_at", n.At.UTC().Format(time.RFC3339))
		}
		fmt.Fprint(bw, "    </node>\n")
	}
	for i, e := range edges {
		fmt.Fprintf(bw, "    <edge id=\"e%d\" source=%q target=%q>\n", i, xmlID[e.From], xmlID[e.To])
		data(bw, "e_kind", e.Kind)
		data(bw, "e_weight", strconv.FormatFloat(e.Weight, 'g', -1, 64))
		data(bw, "e_connector", e.Connector)
		fmt.Fprint(bw, "    </edge>\n")
	}
	fmt.Fprint(bw, "  </graph>\n</graphml>\n")
	return res, bw.Flush()
}

// data skips an empty value: a reader takes an absent attribute as empty.
func data(w *bufio.Writer, key, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(w, "      <data key=%q>", key)
	xml.EscapeText(w, []byte(value))
	fmt.Fprint(w, "</data>\n")
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
