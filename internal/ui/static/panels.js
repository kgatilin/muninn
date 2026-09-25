// The three lists beside the canvas: what `bank show` says, the hits, and the
// selected node. They render from the API's payloads and call back on a click;
// they hold no state.

// h builds an element; text always goes in as text.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props ?? {})) {
    if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== false) el.setAttribute(k, v);
  }
  for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) el.append(c);
  return el;
}

const when = (iso) => new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
const lineRange = (l) => (!l ? "" : l[1] > l[0] ? `:${l[0]}-${l[1]}` : `:${l[0]}`);

export function summary(el, b) {
  el.replaceChildren();
  if (!b) return;
  if (b.error) { el.append(h("div", { class: "error" }, b.error)); return; }
  const line = (cap, ...v) => el.append(h("div", { class: "line" }, h("span", { class: "cap" }, cap), h("span", null, ...v)));
  const c = b.counts;
  line("embedder", b.embedder, b.endpoint ? ` at ${b.endpoint}` : "");
  line("chunker", b.chunker, ...Object.entries(b.budgets ?? {}).map(([n, v]) => `  ${n}.budget=${v}`));
  line("search", `seeds=${b.seeds}`, ...Object.entries(b.edge_weights ?? {}).map(([k, w]) => ` ${k}=${w}`));
  line("index", (b.index_every ? `every ${b.index_every}, ` : "") + `${c.nodes} nodes (${c.text_nodes} with text), ${c.chunks} chunks, ${c.edges} edges, ${c.vectors} vectors`);
  for (const k of b.connectors) {
    const kinds = Object.entries(k.kinds ?? {}).sort(([, a], [, b]) => b - a).map(([kind, n]) => `${n} ${kind}`).join(", ");
    let state = `${k.nodes} nodes` + (kinds ? ` (${kinds}), ` : ", ") + (k.last_run ? `last run ${when(k.last_run)}` : "never run");
    if (k.run_error) state += ` failed: ${k.run_error}`;
    if (k.cursor) state += `, cursor ${k.cursor}`;
    el.append(h("div", { class: "connector" },
      h("div", { class: "line" }, h("span", { class: "cap" }, "connector"), h("span", null, k.name)),
      h("span", { class: "soft" }, k.command.join(" ")),
      h("span", { class: "soft" }, state)));
  }
  if (!b.connectors.length) line("connector", "none");
  // How far each stage has got: a pending count above zero is work the next index does.
  for (const e of b.enrich ?? []) line("enrich", `${e.name}: ${e.items} items, ${e.read} nodes read, ${e.pending} pending`);
  const l = b.layer;
  line("semantic", `${l.entities} entities, ${l.topics} topics, ${l.mentions} mentions; ${l.read} text nodes read, ${l.pending} pending`);
  if (b.compacted) line("compact", `${b.compacted} nodes compacted`);
}

// usage is what the bank's paid model calls came to. An estimate from the
// price table: the title says what it leaves out.
const money = (l) => "$" + l.cost.toFixed(l.cost < 1 ? 4 : 2) + (l.unpriced ? ` +${l.unpriced} unpriced` : "");
const tokens = (n) => (n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n));

export function usage(el, u) {
  el.replaceChildren();
  if (!u || !u.all.calls) return;
  el.title = "estimated from the tokens the providers reported and the price table; failed requests and discounts are not counted";
  const line = (cap, ...v) => el.append(h("div", { class: "line" }, h("span", { class: "cap" }, cap), h("span", null, ...v)));
  line("cost", `${money(u.bank)} this bank, ${money(u.today)} today, ${money(u.all)} all banks`);
  const split = (l) => h("span", { class: "soft" }, `${l.key}  ${money(l)}  ${l.calls} calls, ${tokens(l.in)} in` + (l.out ? `, ${tokens(l.out)} out` : ""));
  for (const l of u.by_purpose) el.append(split(l));
  if (u.by_model.length > 1) for (const l of u.by_model) el.append(split(l));
}

// hitClass is how a hit got its place: it seeded the walk, it matched the text
// without seeding it, or the walk reached it and no text did.
export const hitClass = (hit) => (hit.seed ? "seed" : hit.score > 0 ? "text" : "reach");
const tagOf = { seed: "seed", text: "text", reach: "graph" };

export function hits(el, list, { selected, onSelect }) {
  el.replaceChildren();
  const maxMass = Math.max(...list.map((x) => x.mass), 0);
  const maxScore = Math.max(...list.map((x) => x.score), 0);
  list.forEach((hit, i) => {
    const cls = hitClass(hit);
    // The bar is the PPR mass; with the walk off there is none and it is the
    // fused text score.
    const share = maxMass > 0 ? hit.mass / maxMass : maxScore > 0 ? hit.score / maxScore : 0;
    el.append(h("li", { class: hit.id === selected ? "selected" : "", "data-id": hit.id, title: hit.id, onclick: () => onSelect(hit.id) },
      h("span", { class: "rank" }, String(i + 1)),
      h("div", { class: "body" },
        h("div", { class: "title" },
          h("span", { class: `tag ${cls}` }, tagOf[cls]),
          h("span", { class: "label" }, hit.label),
          h("span", { class: "lines" }, lineRange(hit.lines))),
        hit.heading ? h("div", { class: "heading" }, hit.heading) : h("div", { class: "heading" }, hit.kind),
        hit.snippet && h("div", { class: "snippet" }, hit.snippet),
        hit.also && h("div", { class: "lines" }, "also " + hit.also.map(lineRange).join(" ")),
        h("div", { class: "measure" },
          h("span", { class: "bar" }, h("i", { class: cls, style: `width:${(100 * share).toFixed(1)}%` })),
          h("span", null, maxMass > 0 ? `mass ${hit.mass.toFixed(4)}` : "no walk"),
          h("span", null, `text ${hit.score.toFixed(4)}`)))));
  });
}

// table is the nodes of a kind as rows: when, what it is about, its attrs, and
// its reach in numbers. A header sorts by its column; a row opens the node.
const ATTRS_SHOWN = 4;

export function table(el, nodes, { onOpen, sort }) {
  const count = (get) => nodes.filter((n) => get(n) !== undefined && get(n) !== "").length;
  const attrs = [...new Set(nodes.flatMap((n) => Object.keys(n.attrs ?? {})))]
    .filter((k) => new Set(nodes.map((n) => n.attrs?.[k])).size > 1 && !["started", "session"].includes(k))
    .sort((a, b) => count((n) => n.attrs?.[b]) - count((n) => n.attrs?.[a])).slice(0, ATTRS_SHOWN);
  // A kind every node reaches once, a session's branch, is an attr already.
  const reached = [...new Set(nodes.flatMap((n) => Object.keys(n.reach ?? {})))].filter((k) => nodes.some((n) => n.reach?.[k] > 1)).sort();
  const cols = [
    { name: "when", get: (n) => n.at ?? "", show: (n) => (n.at ? when(n.at) : "") },
    { name: "node", get: (n) => n.about || n.label, title: (n) => n.id },
    ...attrs.map((k) => ({ name: k, get: (n) => n.attrs?.[k] ?? "" })),
    ...(nodes.some((n) => n.parts) ? [{ name: "parts", num: true, get: (n) => n.parts ?? 0 }] : []),
    ...reached.map((k) => ({ name: k, num: true, get: (n) => n.reach?.[k] ?? 0 })),
    { name: "edges", num: true, get: (n) => n.degree },
  ];
  const by = cols.find((c) => c.name === sort.by);
  const rows = by ? [...nodes].sort((a, b) => {
    const x = by.get(a), y = by.get(b);
    return (x < y ? -1 : x > y ? 1 : 0) * (sort.up ? 1 : -1);
  }) : nodes;
  el.replaceChildren(h("table", null,
    h("thead", null, h("tr", null, cols.map((c) => h("th", {
      class: (c.num ? "num" : "") + (c.name === sort.by ? " sorted" + (sort.up ? " up" : "") : ""),
      onclick: () => { sort.up = sort.by === c.name ? !sort.up : false; sort.by = c.name; table(el, nodes, { onOpen, sort }); },
    }, c.name)))),
    h("tbody", null, rows.map((n) => h("tr", { title: n.id, onclick: () => onOpen(n.id) },
      cols.map((c) => h("td", { class: c.num ? "num" : "", title: c.title?.(n) }, String((c.show ?? c.get)(n))))))))); 
}

// around is a node's surroundings as lists, smallest group first: a session's
// rules, then its topics and entities, each with its text. It stays while the
// nodes in it are opened; the one opened shows its text whole.
// An entity's text opens with its name, which the title has said.
const body_ = (nb) => { const t = (nb.text ?? "").trim(); return t === nb.label ? "" : t.startsWith(nb.label + "\n") ? t.slice(nb.label.length).trim() : t; };

export function around(el, n, { colorOf, selected, onSelect, onClose }) {
  el.replaceChildren();
  if (!n || n.error) { if (n) el.append(h("p", { class: "empty error" }, n.error)); return; }
  const body = h("div", { class: "body" });
  const fill = (want) => {
    body.replaceChildren(...[...(n.reach?.groups ?? [])].sort((a, b) => a.total - b.total).map((g) => {
      const items = g.neighbours.filter((nb) => !want || (nb.label + "\n" + (nb.text ?? "")).toLowerCase().includes(want));
      return items.length && h("details", { open: "" },
        h("summary", null, (g.dir === "in" ? `← ${g.kind} ${g.node_kind}  ` : `${g.kind} → ${g.node_kind}  `) + `${want ? items.length + " of " : ""}${g.total}` + (g.total > g.neighbours.length ? `, the first ${g.neighbours.length}` : "")),
        items.map((nb) => h("div", { class: "item" + (nb.id === selected ? " selected" : ""), "data-id": nb.id, title: nb.id, onclick: () => onSelect(nb.id) },
          h("div", { class: "title" }, h("i", { style: `--c:${colorOf("node", nb.kind)}` }), h("span", { class: "label" }, nb.label), h("span", { class: "soft" }, `×${nb.weight}`)),
          body_(nb) && h("div", { class: "text" }, body_(nb)))));
    }).filter(Boolean));
    if (!body.children.length) body.append(h("p", { class: "empty" }, want ? "nothing holds that" : "nothing is tied to it"));
  };
  el.append(h("div", { class: "head" },
    h("div", { class: "title", title: n.id, onclick: () => onSelect(n.id) },
      h("span", { class: "label" }, n.label), h("span", { class: "soft" }, n.kind), h("button", { type: "button", title: "back to the whole bank", onclick: (ev) => { ev.stopPropagation(); onClose(); } }, "×")),
    h("span", { class: "soft" }, [n.at && when(n.at), n.reach?.parts && `${n.reach.parts} parts`, ...Object.entries(n.attrs ?? {}).filter(([k]) => ["branch", "tool"].includes(k)).map(([, v]) => v)].filter(Boolean).join(" · ")),
    h("input", { type: "search", placeholder: "holds…", oninput: (ev) => fill(ev.target.value.trim().toLowerCase()) })), body);
  fill("");
}

export function markSelected(el, id) {
  for (const item of el.querySelectorAll?.(".item") ?? []) item.classList.toggle("selected", item.dataset.id === id);
  for (const li of el.children) li.classList.toggle("selected", li.dataset.id === id);
}

export function node(el, n, { colorOf, onSelect }) {
  el.replaceChildren();
  if (!n) { el.append(h("p", { class: "empty" }, "Select a node.")); return; }
  if (n.error) { el.append(h("p", { class: "empty error" }, n.error)); return; }
  el.append(h("h2", null, n.label), h("div", { class: "id" }, n.id));

  const rows = [["kind", n.kind], ["connector", n.connector], ["at", n.at && when(n.at)], ["degree", String(n.degree)],
    ...Object.entries(n.attrs ?? {}).sort(([a], [b]) => a.localeCompare(b))].filter(([, v]) => v);
  el.append(h("section", null, h("span", { class: "cap" }, "attrs"),
    h("table", null, rows.map(([k, v]) => h("tr", null, h("td", null, k), h("td", null, v))))));

  if (n.chunks.length) {
    el.append(h("section", null, h("span", { class: "cap" }, `chunks · ${n.chunks.length}`),
      n.chunks.map((c, i) => h("div", { class: "chunk" },
        h("div", { class: "where" }, h("span", { class: "mono" }, `${i + 1}${lineRange(c.lines)}`), h("span", null, c.prefix ?? "")),
        h("pre", null, c.text)))));
  } else {
    el.append(h("section", null, h("span", { class: "cap" }, "chunks"), h("span", { class: "soft" }, "a structural node: it has no text")));
  }

  if (n.edges.length) {
    el.append(h("section", null, h("span", { class: "cap" }, "neighbours"),
      n.edges.map((g) => h("div", { class: "group" },
        h("div", { class: "name" }, g.dir === "out" ? `${g.kind} → ${g.neighbours.length}` : `← ${g.kind} ${g.neighbours.length}`),
        h("ul", null, g.neighbours.map((nb) => h("li", { title: nb.id, onclick: () => onSelect(nb.id) },
          h("i", { style: `--c:${colorOf("node", nb.kind)}` }),
          h("span", { class: "label" }, nb.label),
          h("span", { class: "soft" }, nb.kind))))))));
  }

  // What the node's parts are tied to: the entities, rules and files of a
  // branch's sessions and their messages. The number is how many parts are.
  if (n.reach?.parts) {
    el.append(h("section", null, h("span", { class: "cap" }, `through its ${n.reach.parts} parts`),
      n.reach.groups.map((g) => h("div", { class: "group" },
        h("div", { class: "name" }, (g.dir === "in" ? `← ${g.kind} ${g.node_kind} ${g.total}` : `${g.kind} → ${g.node_kind} ${g.total}`) + (g.total > g.neighbours.length ? `, the first ${g.neighbours.length}` : "")),
        h("ul", null, g.neighbours.map((nb) => h("li", { title: nb.id, onclick: () => onSelect(nb.id) },
          h("i", { style: `--c:${colorOf("node", nb.kind)}` }),
          h("span", { class: "label" }, nb.label),
          h("span", { class: "soft" }, String(nb.weight)))))))));
  }
}
