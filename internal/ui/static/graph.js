// The bank's graph on a canvas. A bank has thousands of nodes, so everything
// is drawn in a handful of paths per frame — one per node kind, one per edge
// kind — and only the lit nodes and the labels are drawn one at a time.
//
// The simulation runs over what is visible: hiding an edge kind takes its
// springs away, so the layout that follows shows what the remaining edges say.

import { forceSimulation, forceLink, forceManyBody, forceX, forceY } from "./vendor/d3-force.js";

const DIM = 0.13;
const LABELS_WHEN_ZOOMED = 90; // label everything in view once this few nodes are

export class GraphView {
  constructor(canvas, { onSelect, colorOf }) {
    this.canvas = canvas;
    this.ctx = canvas.getContext("2d");
    this.onSelect = onSelect;
    this.colorOf = colorOf; // (group: "node" | "edge", kind) -> css colour
    this.nodes = [];
    this.edges = [];
    this.byId = new Map();
    this.hiddenNodes = new Set();
    this.hiddenEdges = new Set();
    this.hits = null; // Map id -> {cls, weight}
    this.focus = false;
    this.selected = null;
    this.hover = null;
    this.view = { x: 0, y: 0, k: 1 };
    this.fitted = false;
    this.vnodes = [];
    this.vedges = [];
    this.visible = new Set();

    this.sim = forceSimulation([])
      .force("link", forceLink([]).distance(34).strength(0.35))
      .force("charge", forceManyBody().strength(-42).theta(1.1).distanceMax(420))
      .force("x", forceX(0).strength(0.035))
      .force("y", forceY(0).strength(0.035))
      .alphaDecay(0.025)
      .on("tick", () => {
        // The camera follows a new layout while it settles, until the user
        // moves it.
        if (!this.fitted) { this.fit(); this.fitted = this.sim.alpha() < 0.05; }
        this.draw();
      });
    this.sim.stop();

    this.restyle();
    new ResizeObserver(() => this.resize()).observe(canvas.parentElement);
    matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => this.restyle());
    this.bind();
    this.resize();
  }

  // ---- data ---------------------------------------------------------------

  // setData keeps the position of every node it already had, and puts a new
  // node beside a neighbour that has one, so a refetch does not reshuffle.
  setData({ nodes, edges }, { keepView = false } = {}) {
    const old = this.byId;
    this.byId = new Map();
    this.nodes = nodes.map((n) => {
      const was = old.get(n.id);
      const node = { ...n, x: was?.x, y: was?.y, vx: 0, vy: 0 };
      this.byId.set(n.id, node);
      return node;
    });
    this.edges = [];
    for (const e of edges) {
      const source = this.byId.get(e.from), target = this.byId.get(e.to);
      if (source && target && source !== target) this.edges.push({ source, target, kind: e.kind });
    }
    for (const e of this.edges) {
      for (const [a, b] of [[e.source, e.target], [e.target, e.source]]) {
        if (a.x === undefined && b.x !== undefined) {
          a.x = b.x + (Math.random() - 0.5) * 30;
          a.y = b.y + (Math.random() - 0.5) * 30;
        }
      }
    }
    if (!keepView) this.fitted = false;
    if (this.selected && !this.byId.has(this.selected)) this.selected = null;
    this.hover = null;
    this.refresh(keepView ? 0.3 : 1);
  }

  setHidden(nodeKinds, edgeKinds) {
    this.hiddenNodes = nodeKinds;
    this.hiddenEdges = edgeKinds;
    this.refresh(0.5);
  }

  // hits: Map id -> {cls: "seed" | "text" | "reach", weight: 0..1}, or null.
  setHits(hits) {
    this.hits = hits && hits.size ? hits : null;
    if (!this.hits) this.focus = false;
    this.refresh(this.focus ? 0.5 : 0);
  }

  setFocus(on) {
    this.focus = on && !!this.hits;
    this.fitted = false;
    this.refresh(0.6);
  }

  select(id, { centre = false } = {}) {
    this.selected = id;
    const n = this.byId.get(id);
    if (centre && n && n.x !== undefined) {
      const { width, height } = this.size();
      this.view.x = width / 2 - n.x * this.view.k;
      this.view.y = height / 2 - n.y * this.view.k;
    }
    this.draw();
  }

  has(id) { return this.byId.has(id); }

  // refresh works out what is visible and hands that to the simulation.
  refresh(alpha) {
    const shown = (e) => !this.hiddenEdges.has(e.kind) && !this.hiddenNodes.has(e.source.kind) && !this.hiddenNodes.has(e.target.kind);
    let keep = null;
    if (this.focus) {
      keep = new Set(this.hits.keys());
      for (const e of this.edges) {
        if (!shown(e)) continue;
        if (this.hits.has(e.source.id)) keep.add(e.target.id);
        if (this.hits.has(e.target.id)) keep.add(e.source.id);
      }
    }
    this.vnodes = this.nodes.filter((n) => !this.hiddenNodes.has(n.kind) && (!keep || keep.has(n.id)));
    const visible = this.visible = new Set(this.vnodes);
    this.vedges = this.edges.filter((e) => shown(e) && visible.has(e.source) && visible.has(e.target));

    this.nodeBatches = groupBy(this.vnodes, (n) => n.kind);
    this.edgeBatches = groupBy(this.vedges, (e) => e.kind);

    this.sim.nodes(this.vnodes);
    this.sim.force("link").links(this.vedges);
    if (alpha > 0) this.sim.alpha(Math.max(this.sim.alpha(), alpha)).restart();
    else this.draw();
  }

  // relayout forgets where the nodes are, the ones dragged into place too, and
  // lets the simulation place them again from its start; the view follows.
  relayout() {
    for (const n of this.nodes) {
      delete n.x; delete n.y; delete n.vx; delete n.vy;
      n.fx = n.fy = null;
    }
    this.fitted = false;
    this.sim.nodes(this.vnodes);
    this.sim.force("link").links(this.vedges);
    this.sim.alpha(1).restart();
  }

  // ---- camera -------------------------------------------------------------

  size() { return { width: this.canvas.clientWidth, height: this.canvas.clientHeight }; }

  resize() {
    const { width, height } = this.size();
    const dpr = devicePixelRatio || 1;
    this.canvas.width = Math.max(1, Math.round(width * dpr));
    this.canvas.height = Math.max(1, Math.round(height * dpr));
    this.draw();
  }

  fit() {
    const pts = this.vnodes.filter((n) => n.x !== undefined);
    if (!pts.length) return;
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const n of pts) {
      x0 = Math.min(x0, n.x); x1 = Math.max(x1, n.x);
      y0 = Math.min(y0, n.y); y1 = Math.max(y1, n.y);
    }
    const { width, height } = this.size();
    const k = Math.min(4, 0.92 * Math.min(width / Math.max(x1 - x0, 60), height / Math.max(y1 - y0, 60)));
    this.view = { k, x: width / 2 - k * (x0 + x1) / 2, y: height / 2 - k * (y0 + y1) / 2 };
    this.draw();
  }

  // ---- drawing ------------------------------------------------------------

  restyle() {
    const css = getComputedStyle(document.documentElement);
    const v = (name) => css.getPropertyValue(name).trim();
    this.style = {
      ink: v("--ink"), soft: v("--ink-soft"), bg: v("--bg"),
      seed: v("--seed"), text: v("--text"), reach: v("--reach"),
      edgeAlpha: parseFloat(v("--edge-alpha")) || 0.35,
    };
    this.draw();
  }

  // A node's radius in world units. It grows slower than the zoom, so zooming
  // in pulls a cluster apart instead of inflating it.
  radius(n) {
    const hit = this.hits?.get(n.id);
    const px = hit ? 4.5 + 11 * Math.sqrt(hit.weight) : Math.min(13, 2.2 + 0.85 * Math.sqrt(n.degree));
    return px / Math.pow(this.view.k, 0.6);
  }

  draw() {
    if (this.pending) return;
    this.pending = requestAnimationFrame(() => { this.pending = 0; this.paint(); });
  }

  paint() {
    const { ctx, view, style } = this;
    if (!style) return;
    const dpr = devicePixelRatio || 1;
    const { width, height } = this.size();
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, this.canvas.width, this.canvas.height);
    ctx.setTransform(dpr * view.k, 0, 0, dpr * view.k, dpr * view.x, dpr * view.y);

    const lit = this.hits;
    // The selected node and its neighbours stand out: everything else is
    // dimmed, as it is around the hits of a search.
    const near = this.selected ? new Set([this.selected]) : null;
    if (near) {
      for (const [, edges] of this.edgeBatches ?? []) {
        for (const e of edges) {
          if (e.source.id === this.selected || e.target.id === this.selected) near.add(e.source.id).add(e.target.id);
        }
      }
    }
    const dimmed = lit || (near && near.size > 1);

    ctx.lineWidth = 0.8 / view.k;
    for (const [kind, edges] of this.edgeBatches ?? []) {
      ctx.globalAlpha = style.edgeAlpha * (dimmed ? 0.45 : 1);
      ctx.strokeStyle = this.colorOf("edge", kind);
      ctx.beginPath();
      for (const e of edges) { ctx.moveTo(e.source.x, e.source.y); ctx.lineTo(e.target.x, e.target.y); }
      ctx.stroke();
    }
    // Edges between lit nodes, and the selected node's own, at full strength.
    ctx.globalAlpha = 0.9;
    ctx.lineWidth = 1.3 / view.k;
    for (const [kind, edges] of this.edgeBatches ?? []) {
      ctx.strokeStyle = this.colorOf("edge", kind);
      ctx.beginPath();
      for (const e of edges) {
        const ofSelected = e.source.id === this.selected || e.target.id === this.selected;
        if (ofSelected || (lit && lit.has(e.source.id) && lit.has(e.target.id))) {
          ctx.moveTo(e.source.x, e.source.y); ctx.lineTo(e.target.x, e.target.y);
        }
      }
      ctx.stroke();
    }

    for (const [kind, nodes] of this.nodeBatches ?? []) {
      ctx.globalAlpha = dimmed ? DIM : 1;
      ctx.fillStyle = this.colorOf("node", kind);
      ctx.beginPath();
      for (const n of nodes) {
        if (dimmed && (lit?.has(n.id) || near?.has(n.id))) continue;
        const r = this.radius(n);
        ctx.moveTo(n.x + r, n.y); ctx.arc(n.x, n.y, r, 0, 6.2832);
      }
      ctx.fill();
    }
    ctx.globalAlpha = 1;
    if (dimmed) {
      // The selected node's neighbours keep their colour, with a rim that
      // tells them from the dimmed ones at a glance.
      for (const id of near ?? []) {
        const n = this.byId.get(id);
        if (!n || lit?.has(id) || !this.visible.has(n)) continue;
        ctx.fillStyle = this.colorOf("node", n.kind);
        ctx.beginPath(); ctx.arc(n.x, n.y, this.radius(n), 0, 6.2832); ctx.fill();
        ctx.lineWidth = 1 / view.k; ctx.strokeStyle = style.ink; ctx.stroke();
      }
    }
    if (lit) {
      // A seed and a text hit are filled; a node the walk reached is a ring
      // around its kind's colour.
      const ordered = this.vnodes.filter((n) => lit.has(n.id)).sort((a, b) => lit.get(a.id).weight - lit.get(b.id).weight);
      for (const n of ordered) {
        const { cls } = lit.get(n.id), r = this.radius(n);
        ctx.beginPath(); ctx.arc(n.x, n.y, r, 0, 6.2832);
        if (cls === "reach") {
          ctx.fillStyle = this.colorOf("node", n.kind); ctx.fill();
          ctx.lineWidth = 2.4 / view.k; ctx.strokeStyle = style.reach; ctx.stroke();
        } else {
          ctx.fillStyle = style[cls]; ctx.fill();
          ctx.lineWidth = 1 / view.k; ctx.strokeStyle = style.bg; ctx.stroke();
        }
      }
    }
    for (const id of [this.hover, this.selected]) {
      const n = id && this.byId.get(id);
      if (!n || n.x === undefined || !this.visible.has(n)) continue;
      ctx.beginPath(); ctx.arc(n.x, n.y, this.radius(n) + 3 / view.k, 0, 6.2832);
      ctx.lineWidth = (id === this.selected ? 2 : 1.2) / view.k;
      ctx.strokeStyle = style.ink; ctx.stroke();
    }

    // Labels, in screen space.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.font = "11px ui-monospace, 'SF Mono', Menlo, monospace";
    ctx.textBaseline = "middle";
    const labelled = new Set();
    const put = (n, strong) => {
      if (!n || labelled.has(n.id) || n.x === undefined) return;
      const x = n.x * view.k + view.x, y = n.y * view.k + view.y;
      if (x < -50 || y < -10 || x > width + 50 || y > height + 10) return;
      labelled.add(n.id);
      const tx = x + this.radius(n) * view.k + 4;
      ctx.lineWidth = 3; ctx.strokeStyle = style.bg; ctx.globalAlpha = 0.85;
      ctx.strokeText(n.label, tx, y);
      ctx.globalAlpha = 1; ctx.fillStyle = strong ? style.ink : style.soft;
      ctx.fillText(n.label, tx, y);
    };
    const visible = this.visible;
    for (const id of [this.hover, this.selected]) if (visible.has(this.byId.get(id))) put(this.byId.get(id), true);
    if (lit) for (const n of this.vnodes) if (lit.has(n.id)) put(n, true);
    if (near) for (const id of near) if (near.size <= 40 && visible.has(this.byId.get(id))) put(this.byId.get(id), false);
    if (view.k > 1.6) {
      const inView = this.vnodes.filter((n) => {
        const x = n.x * view.k + view.x, y = n.y * view.k + view.y;
        return x > 0 && y > 0 && x < width && y < height;
      });
      if (inView.length <= LABELS_WHEN_ZOOMED) for (const n of inView) put(n, false);
    }
  }

  // ---- pointer ------------------------------------------------------------

  at(ev) {
    const box = this.canvas.getBoundingClientRect();
    const x = (ev.clientX - box.left - this.view.x) / this.view.k;
    const y = (ev.clientY - box.top - this.view.y) / this.view.k;
    let best = null, bestD = Infinity;
    for (const n of this.vnodes) {
      const d = Math.hypot(n.x - x, n.y - y) - this.radius(n);
      // Under a search the lit nodes win a tie with the dimmed ones around them.
      const bias = this.hits && !this.hits.has(n.id) ? 2 / this.view.k : 0;
      if (d + bias < bestD) { bestD = d + bias; best = n; }
    }
    return { x, y, node: bestD <= 4 / this.view.k ? best : null };
  }

  bind() {
    const c = this.canvas;
    let drag = null;

    c.addEventListener("pointerdown", (ev) => {
      const { x, y, node } = this.at(ev);
      drag = { node, x, y, sx: ev.clientX, sy: ev.clientY, vx: this.view.x, vy: this.view.y, moved: false };
      c.setPointerCapture(ev.pointerId);
      c.classList.add("dragging");
    });
    c.addEventListener("pointermove", (ev) => {
      if (!drag) {
        const { node } = this.at(ev);
        const id = node?.id ?? null;
        if (id !== this.hover) { this.hover = id; c.classList.toggle("over", !!id); this.draw(); }
        return;
      }
      if (Math.hypot(ev.clientX - drag.sx, ev.clientY - drag.sy) > 3) drag.moved = true;
      if (!drag.moved) return;
      if (drag.node) {
        const p = this.at(ev);
        drag.node.fx = p.x; drag.node.fy = p.y;
        this.sim.alphaTarget(0.15).restart();
      } else {
        this.fitted = true;
        this.view.x = drag.vx + ev.clientX - drag.sx;
        this.view.y = drag.vy + ev.clientY - drag.sy;
        this.draw();
      }
    });
    const end = () => {
      if (!drag) return;
      if (drag.node) { drag.node.fx = drag.node.fy = null; this.sim.alphaTarget(0); }
      if (!drag.moved) this.onSelect(drag.node?.id ?? null);
      drag = null;
      c.classList.remove("dragging");
    };
    c.addEventListener("pointerup", end);
    c.addEventListener("pointercancel", end);
    c.addEventListener("pointerleave", () => { if (!drag && this.hover) { this.hover = null; this.draw(); } });

    c.addEventListener("wheel", (ev) => {
      ev.preventDefault();
      this.fitted = true;
      const box = c.getBoundingClientRect();
      const px = ev.clientX - box.left, py = ev.clientY - box.top;
      const k = Math.min(24, Math.max(0.03, this.view.k * Math.exp(-ev.deltaY * (ev.ctrlKey ? 0.01 : 0.0018))));
      this.view.x = px - (px - this.view.x) * (k / this.view.k);
      this.view.y = py - (py - this.view.y) * (k / this.view.k);
      this.view.k = k;
      this.draw();
    }, { passive: false });
  }
}

function groupBy(items, key) {
  const out = new Map();
  for (const it of items) {
    const k = key(it);
    if (!out.has(k)) out.set(k, []);
    out.get(k).push(it);
  }
  return out;
}
