// The page's state and the wiring between the panels, the canvas and the API.

import * as api from "./api.js";
import * as panels from "./panels.js";
import { GraphView } from "./graph.js";

const $ = (id) => document.getElementById(id);
const NODE_COLOURS = 8, EDGE_COLOURS = 6;

const state = {
  banks: [],
  bank: null,
  graph: null,      // the last graph payload
  hits: [],
  selected: null,
  list: false,      // the centre shows the list, not the drawing
  around: null,     // the node whose surroundings alone are drawn
  hidden: { node: new Set(), edge: new Set() },
  colour: { node: new Map(), edge: new Map() }, // kind -> index into the palette
};

// A kind's colour is its place in the bank's kinds ordered by count, so the
// commonest kinds get the first, most distinct colours.
const cssOf = (group, kind) => {
  const i = state.colour[group].get(kind) ?? 0;
  return group === "node" ? `var(--k${i % NODE_COLOURS})` : `var(--e${i % EDGE_COLOURS})`;
};
let resolved = new Map();
const colorOf = (group, kind) => {
  const name = cssOf(group, kind).slice(4, -1);
  if (!resolved.has(name)) resolved.set(name, getComputedStyle(document.documentElement).getPropertyValue(name).trim());
  return resolved.get(name);
};
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { resolved = new Map(); });

const view = new GraphView($("graph"), { colorOf, onSelect: (id) => select(id) });

// ---- banks ------------------------------------------------------------------

async function start() {
  try {
    state.banks = await api.banks();
  } catch (err) {
    $("summary").replaceChildren(panels.h("div", { class: "error" }, err.message));
    return;
  }
  const picker = $("bank");
  picker.replaceChildren(...state.banks.map((b) => panels.h("option", { value: b.name }, b.name)));
  if (!state.banks.length) {
    $("summary").replaceChildren(panels.h("div", { class: "soft" }, "no banks yet — `muninn bank add <name>`"));
    return;
  }
  await restore();
}

// The hash carries where the page is: the bank, the query, the list and its
// kind, the node whose surroundings are drawn, the node selected. Every step
// is an entry of the browser's history, so Back undoes it.
let silent = 0;
function remember() {
  if (silent) return;
  const p = new URLSearchParams({ bank: state.bank });
  if (state.hits.length) p.set("q", $("q").value.trim());
  if (state.list) { p.set("list", $("listkind").value); if ($("listq").value.trim()) p.set("lq", $("listq").value.trim()); }
  if (state.around) p.set("around", state.around);
  if (state.selected) p.set("sel", state.selected);
  if ("#" + p !== location.hash) history.pushState(null, "", "#" + p);
}

// step is one move of the user's made of several of the page's: one entry.
async function step(fn) {
  silent++;
  try { await fn(); } finally { silent--; }
  remember();
}

// restore brings the page to what the hash says, and writes no entry.
async function restore() {
  const p = new URLSearchParams(location.hash.slice(1));
  silent++;
  try {
    const bank = state.banks.some((b) => b.name === p.get("bank")) ? p.get("bank") : state.bank ?? state.banks[0].name;
    if (bank !== state.bank) { $("bank").value = bank; await openBank(bank); }
    if ((p.get("q") ?? "") !== (state.hits.length ? $("q").value.trim() : "")) { $("q").value = p.get("q") ?? ""; await runSearch(); }
    if ((p.get("around") ?? null) !== state.around) {
      state.around = p.get("around");
      $("whole").hidden = !state.around;
      showAround();
      await loadGraph({ keepView: false });
      if (state.around) view.relayout();
    }
    if (p.get("list")) $("listkind").value = p.get("list");
    $("listq").value = p.get("lq") ?? "";
    setMode(p.has("list"));
    if ((p.get("sel") ?? null) !== state.selected) await select(p.get("sel"), { centre: true });
  } finally { silent--; }
  if (!location.hash) history.replaceState(null, "", "#" + new URLSearchParams({ bank: state.bank }));
}

// A search on a hosted embedder is itself a paid call, so this is asked again
// after each one. The panel is a report: a failure leaves it empty.
async function showUsage() {
  const bank = state.bank;
  const u = await api.usage(bank).catch(() => null);
  if (bank === state.bank) panels.usage($("usage"), u);
}

async function openBank(name) {
  state.bank = name;
  state.hits = [];
  state.selected = null;
  state.hidden = { node: new Set(), edge: new Set() };
  remember();
  panels.summary($("summary"), state.banks.find((b) => b.name === name));
  showUsage();
  panels.hits($("hits"), [], {});
  panels.node($("node"), null, {});
  $("searchnote").textContent = "";
  $("listq").value = "";
  setMode(false);
  $("whole").hidden = true;
  state.around = null;
  showAround();
  $("focus").checked = false;
  $("focus").disabled = true;
  $("legend").classList.remove("on");
  view.setHits(null);
  view.select(null);
  await loadGraph({ keepView: false });
}

// loadGraph asks for the top of the bank by degree among the node kinds that
// are switched on, plus the hits and the selected node with their neighbours,
// which the server adds over the limit.
async function loadGraph({ keepView }) {
  const withIds = [...state.hits.map((x) => x.id), ...(state.selected ? [state.selected] : [])];
  try {
    state.graph = await api.graph(state.bank, { limit: $("limit").value || 0, withIds, hide: [...state.hidden.node], around: state.around });
    state.loadedHidden = [...state.hidden.node].sort().join("\n");
  } catch (err) {
    $("graphnote").textContent = err.message;
    return;
  }
  const g = state.graph;
  for (const [group, counts] of [["node", g.node_kinds], ["edge", g.edge_kinds]]) {
    const kinds = Object.keys(counts).sort((a, b) => counts[b] - counts[a] || a.localeCompare(b));
    state.colour[group] = new Map(kinds.map((k, i) => [k, i]));
  }
  chips();
  const kinds = $("hitkind"), was = kinds.value;
  kinds.replaceChildren(panels.h("option", { value: "" }, "any kind"), ...Object.keys(g.node_kinds).sort().map((k) => panels.h("option", { value: k }, k)));
  kinds.value = was in g.node_kinds ? was : "";
  const listed = $("listkind"), had = listed.value;
  listed.replaceChildren(...Object.keys(g.node_kinds).sort().map((k) => panels.h("option", { value: k }, `${k} ${g.node_kinds[k]}`)));
  listed.value = had in g.node_kinds ? had : "session" in g.node_kinds ? "session" : listed.options[0]?.value ?? "";
  $("graphnote").textContent = g.total_nodes === 0 ? "the bank is empty; `muninn index " + state.bank + "`"
    : (state.around ? "around " + (g.nodes.find((n) => n.id === state.around)?.label ?? state.around) + " · " : "") + `${g.nodes.length} of ${g.total_nodes} nodes, ${g.edges.length} of ${g.total_edges} edges` + (g.truncated ? " · highest degree first" : "");
  view.setData(g, { keepView });
}

function chips() {
  for (const [group, el, counts] of [["node", $("nodekinds"), state.graph.node_kinds], ["edge", $("edgekinds"), state.graph.edge_kinds]]) {
    const kinds = [...state.colour[group].keys()];
    el.replaceChildren(...kinds.map((kind) => panels.h("span", {
      class: `chip ${group}` + (state.hidden[group].has(kind) ? " off" : ""),
      style: `--c:${cssOf(group, kind)}`,
      title: "click to hide or show; cmd-click or alt-click to show only this kind, and again to show all",
      onclick: (ev) => toggle(group, kind, kinds, ev.metaKey || ev.altKey),
    }, panels.h("i"), kind || "(none)", panels.h("span", { class: "n" }, String(counts[kind])))));
  }
}

function toggle(group, kind, kinds, only) {
  const hidden = state.hidden[group];
  if (only) {
    const alone = kinds.every((k) => (k === kind) !== hidden.has(k));
    hidden.clear();
    if (!alone) kinds.filter((k) => k !== kind).forEach((k) => hidden.add(k));
  } else if (!hidden.delete(kind)) {
    hidden.add(kind);
  }
  chips();
  view.setHidden(new Set(state.hidden.node), new Set(state.hidden.edge));
  // A payload cut at the limit was chosen among the kinds switched on when it
  // was asked for; with other kinds on, other nodes are the top of the bank.
  if (group === "node" && (state.graph.truncated || state.loadedHidden !== "") && [...hidden].sort().join("\n") !== state.loadedHidden) {
    loadGraph({ keepView: true });
  }
}

// ---- search -----------------------------------------------------------------

async function runSearch() {
  const q = $("q").value.trim();
  if (!q) {
    state.hits = [];
    remember();
    panels.hits($("hits"), [], {});
    $("searchnote").textContent = "";
    $("focus").checked = false;
    $("focus").disabled = true;
    $("legend").classList.remove("on");
    view.setHits(null);
    return;
  }
  $("searchnote").textContent = "searching…";
  let res;
  try {
    res = await api.search(state.bank, { q, k: $("k").value, nograph: !$("walk").checked, kind: $("hitkind").value });
  } catch (err) {
    $("searchnote").textContent = err.message;
    return;
  }
  state.hits = res.hits;
  remember();
  showUsage();
  $("searchnote").textContent = res.warning || (res.hits.length ? "" : "no hits");
  panels.hits($("hits"), res.hits, { selected: state.selected, onSelect: (id) => select(id, { centre: true }) });
  $("focus").disabled = !res.hits.length;
  $("legend").classList.toggle("on", res.hits.length > 0);

  // A cut graph is fetched again with the hits, so they and their neighbours are in it.
  if (state.graph.truncated) await loadGraph({ keepView: true });
  const maxMass = Math.max(...res.hits.map((x) => x.mass), 0), maxScore = Math.max(...res.hits.map((x) => x.score), 0);
  view.setHits(new Map(res.hits.map((x) => [x.id, {
    cls: panels.hitClass(x),
    weight: maxMass > 0 ? x.mass / maxMass : maxScore > 0 ? x.score / maxScore : 0,
  }])));
  if ($("focus").checked) view.setFocus(true);
}

// ---- list -------------------------------------------------------------------

// The nodes of a kind, as a table over the drawing: a branch or a session is
// picked by its name and date. A row opens the drawing of that node's
// surroundings alone.
const sort = { by: "when", up: false };

// setMode switches the centre between the drawing and the list.
function setMode(list) {
  state.list = list;
  $("asgraph").classList.toggle("on", !list);
  $("aslist").classList.toggle("on", list);
  $("browse").hidden = $("table").hidden = !list;
  if (list) runList();
  remember();
}

async function runList() {
  const kind = $("listkind").value;
  if (!kind) return;
  let res;
  try {
    res = await api.nodes(state.bank, { kind, q: $("listq").value.trim() });
  } catch (err) {
    $("table").replaceChildren(panels.h("p", { class: "empty error" }, err.message));
    return;
  }
  if (kind !== $("listkind").value) return;
  $("listnote").textContent = res.nodes.length < res.total ? `the latest ${res.nodes.length} of ${res.total}` : `${res.total}`;
  panels.table($("table"), res.nodes, { sort, onOpen: openAround });
}

// showAround lists the surroundings beside the drawing, in place of the bank's
// summary and the hits, for as long as they are what is drawn.
async function showAround() {
  const id = state.around;
  $("around").hidden = !id;
  $("left").classList.toggle("around", !!id);
  if (!id) return;
  const n = await api.node(state.bank, id).catch((err) => ({ error: err.message }));
  if (id !== state.around) return;
  panels.around($("around"), n, { colorOf: cssOf, selected: state.selected, onSelect: (nb) => select(nb, { centre: true }), onClose: closeAround });
}

// The surroundings are drawn whole, whatever kinds were switched off, and
// laid out afresh: the places the nodes had in the whole bank are far apart.
const openAround = (id) => step(async () => {
  state.around = id;
  state.selected = id;
  state.hidden = { node: new Set(), edge: new Set() };
  view.setHidden(new Set(), new Set());
  setMode(false);
  $("whole").hidden = false;
  await loadGraph({ keepView: false });
  view.relayout();
  showAround();
  await select(id);
});

const closeAround = () => step(async () => {
  state.around = null;
  $("whole").hidden = true;
  showAround();
  await loadGraph({ keepView: false });
});

// ---- selection --------------------------------------------------------------

async function select(id, { centre = false } = {}) {
  state.selected = id;
  panels.markSelected($("hits"), id);
  panels.markSelected($("around"), id);
  if (!id) {
    view.select(null);
    panels.node($("node"), null, {});
    remember();
    return;
  }
  // A neighbour picked in the panel may be outside a cut graph: fetch it in.
  if (!view.has(id)) { await loadGraph({ keepView: true }); centre = true; }
  view.select(id, { centre });
  let n;
  try {
    n = await api.node(state.bank, id);
  } catch (err) {
    n = { error: err.message };
  }
  if (state.selected !== id) return;
  panels.node($("node"), n, { colorOf: cssOf, onSelect: (nb) => select(nb, { centre: true }) });
  $("right").scrollTop = 0;
  remember();
}

// ---- wiring -----------------------------------------------------------------

$("bank").addEventListener("change", (ev) => step(() => openBank(ev.target.value)));
addEventListener("popstate", restore);
$("search").addEventListener("submit", (ev) => { ev.preventDefault(); runSearch(); });
$("q").addEventListener("search", () => { if (!$("q").value) runSearch(); });
$("search").addEventListener("submit", () => setMode(false));
$("asgraph").addEventListener("click", () => setMode(false));
$("aslist").addEventListener("click", () => setMode(true));
$("whole").addEventListener("click", closeAround);
$("listkind").addEventListener("change", () => { runList(); remember(); });
let typing;
$("listq").addEventListener("input", () => { clearTimeout(typing); typing = setTimeout(() => { runList(); remember(); }, 400); });
$("walk").addEventListener("change", () => { if (state.hits.length) runSearch(); });
$("fit").addEventListener("click", () => view.fit());
$("relayout").addEventListener("click", () => view.relayout());
$("focus").addEventListener("change", (ev) => view.setFocus(ev.target.checked));
$("limit").addEventListener("change", () => loadGraph({ keepView: false }));
addEventListener("keydown", (ev) => {
  if (ev.key === "/" && document.activeElement !== $("q")) { ev.preventDefault(); $("q").focus(); }
  if (ev.key === "Escape") select(null);
});

start();
