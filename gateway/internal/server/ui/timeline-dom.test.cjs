// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/timeline-dom.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM, VirtualConsole } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

// jsdom does not perform layout. Supply explicit measurements to exercise
// production panel/connector logic, not to claim browser paint verification.
function panelHarness(t) {
  const dom = new JSDOM(`<div id="timelineScroll"><div class="timeline-group">
    <div class="timeline-row"><div class="timeline-track"></div></div>
    <div class="timeline-detail" hidden><div class="timeline-label"></div>
      <div class="timeline-detail-lane"></div></div>
    <svg class="timeline-leads"></svg>
  </div></div>`);
  t.after(() => dom.window.close());
  const { document } = dom.window;
  const group = document.querySelector(".timeline-group");
  const track = group.querySelector(".timeline-track");
  const lane = group.querySelector(".timeline-detail-lane");
  const detail = group.querySelector(".timeline-detail");
  const svg = group.querySelector("svg");
  const scroller = document.querySelector("#timelineScroll");
  Object.defineProperty(scroller, "clientWidth", { value: 1180 });
  Object.defineProperty(lane, "clientWidth", { value: 1000 });
  Object.defineProperty(track, "offsetWidth", { value: 1000 });
  Object.defineProperty(group, "offsetHeight", {
    get: () => 64 + (detail.hidden ? 0 : parseFloat(lane.style.minHeight)),
  });
  Object.defineProperty(dom.window.HTMLElement.prototype, "offsetWidth", {
    get() { return Number(this.dataset.width || 180); },
  });
  Object.defineProperty(dom.window.HTMLElement.prototype, "offsetHeight", {
    get() { return Number(this.dataset.height || 52); },
  });
  const rect = (left, top, width, height) =>
    ({ left, top, width, height, right: left + width, bottom: top + height });
  svg.getBoundingClientRect = () => rect(200, 100, 1000, group.offsetHeight);
  dom.window.HTMLElement.prototype.getBoundingClientRect = function () {
    const panel = this.classList.contains("timeline-callout");
    const left = Number(this.dataset.visualLeft ?? parseFloat(this.style.left));
    return rect(200 + left, panel ? Number(this.dataset.top || 180) : 110,
      panel ? this.offsetWidth : parseFloat(this.style.width),
      panel ? this.offsetHeight : 54);
  };
  const frames = [];
  let clock = 0;
  const context = vm.createContext({
    document, window: { addEventListener() {} }, setInterval() {},
    $: (selector) => document.querySelector(selector),
    getComputedStyle: () => ({ getPropertyValue: () => "180px" }),
    requestAnimationFrame: (fn) => { frames.push(fn); return frames.length; },
    performance: { now: () => clock },
    esc: (value) => String(value ?? "").replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;").replaceAll(">", "&gt;"),
    time: (ts) => String(ts),
  });
  vm.runInContext(script.slice(script.indexOf("      const TL_HOUR"),
    script.indexOf("      function askCard")), context);
  const run = (code) => vm.runInContext(code, context);
  function add(id, kind, ts, left = 700, text = id) {
    const item = { kind, events: [{ id, kind, ts, text }] };
    const key = `block:project:${id}`;
    context.testItem = item;
    run(`timeline.items.set(${JSON.stringify(key)}, testItem)`);
    const block = document.createElement("button");
    block.className = "timeline-block";
    block.dataset.timelineKey = key;
    block.style.left = `${left}px`;
    block.style.width = "8px";
    track.append(block);
    return { item, block, key };
  }
  return {
    run, add, group, track, lane, detail, svg, scroller, frames,
    layout: (now = 100000) => run(`layoutTimelinePanels(${now})`),
    panels: () => [...lane.children],
    frame(ms) { clock = ms; frames.splice(0).forEach((fn) => fn(ms)); },
  };
}

test("panels use newest merged-run text and reuse their element when the run grows", (t) => {
  const h = panelHarness(t);
  const a = h.add("a", "progress", 98000);
  a.item.events.push({ id: "b", kind: "progress", ts: 99000, text: "<newest>" });
  h.layout();
  const panel = h.panels()[0];
  assert.equal(h.panels().length, 1);
  assert.equal(panel.dataset.runKey, a.key);
  assert.equal(panel.querySelector(".timeline-callout-text").textContent, "<newest>");
  assert.match(panel.textContent, /progress · 2/);
  assert.equal(panel.querySelector("newest"), null, "text is escaped");
  const left = panel.style.left;
  a.item.events.push({ id: "c", kind: "progress", ts: 100000, text: "x".repeat(300) });
  h.layout();
  assert.equal(h.panels()[0], panel);
  assert.equal(panel.style.left, left);
  assert.equal(panel.querySelector(".timeline-callout-text").textContent.length, 240);
  assert.match(panel.textContent, /progress · 3/);
});

test("panel eligibility uses old board kinds, visibility and the exact 60s boundary", (t) => {
  const h = panelHarness(t);
  for (const kind of ["progress", "milestone", "note", "poke", "waiting", "host", "reminder", "active", "idle"])
    h.add(kind, kind, 100000);
  h.add("expired", "note", 40000);
  h.add("barely-fresh", "note", 40001);
  h.add("future", "note", 100001);
  h.add("offscreen", "note", 99000, 1100);
  h.add("hidden", "note", 99000).block.hidden = true;
  h.layout();
  assert.deepEqual(h.panels().map((p) => p.dataset.runKey.split(":").at(-1)),
    ["progress", "milestone", "note", "poke", "waiting", "barely-fresh"]);
  assert.equal(h.detail.querySelector(".timeline-label").textContent, "6 new");
});

test("panels freeze their initial x and newer panels shove older ones left with 6px gaps", (t) => {
  const h = panelHarness(t);
  const a = h.add("a", "note", 97000);
  h.layout();
  const panel = h.panels()[0];
  assert.equal(parseFloat(panel.style.left), 614, "centred below 8px block at x=700");
  a.block.style.left = "690px";
  h.layout(101000);
  assert.equal(parseFloat(panel.style.left), 614, "clock movement does not move the panel");
  h.add("b", "milestone", 98000);
  h.add("c", "waiting", 99000);
  h.layout();
  assert.deepEqual(h.panels().map((p) => parseFloat(p.style.left)), [242, 428, 614]);
  assert.equal(h.frames.length, 1, "a connector animation is scheduled for the shove");
});

test("measured panel heights expand the detail row; fade and expiry contract it", (t) => {
  const h = panelHarness(t);
  h.add("short", "note", 90000);
  h.add("tall", "waiting", 50000);
  h.layout();
  const [short, tall] = h.panels();
  assert.equal(h.detail.hidden, false);
  assert.equal(h.lane.style.minHeight, "66px");
  tall.dataset.height = "92";
  h.layout(95000); // tall is 45 seconds old: fully opaque
  assert.equal(tall.style.opacity, "1");
  assert.equal(h.lane.style.minHeight, "106px");
  h.layout(102500);
  assert.equal(tall.style.opacity, "0.5");
  const lead = h.svg.querySelector(".waiting");
  assert.equal(lead.getAttribute("opacity"), "0.375");
  h.layout(109999);
  assert.equal(tall.style.opacity, "0.2");
  h.layout(110000);
  assert.deepEqual(h.panels(), [short]);
  assert.equal(h.lane.style.minHeight, "66px");
  assert.equal(h.detail.querySelector(".timeline-label").textContent, "new");
  h.layout(150000);
  assert.equal(h.detail.hidden, true);
  assert.equal(h.svg.children.length, 0);
  assert.equal(h.run("timeline.panels.size + timeline.panelX.size"), 0);
});

test("offscreen or collapsed groups lose panels and recreate them when visible", (t) => {
  const h = panelHarness(t);
  h.add("a", "note", 99000);
  h.layout();
  assert.equal(h.panels().length, 1);
  h.scroller.scrollLeft = 800;
  h.layout();
  assert.equal(h.detail.hidden, true);
  assert.equal(h.panels().length, 0);
  h.scroller.scrollLeft = 0;
  h.layout();
  assert.equal(h.panels().length, 1);
  h.group.classList.add("collapsed");
  h.layout();
  assert.equal(h.detail.hidden, true);
  assert.equal(h.svg.children.length, 0);
});

test("group-level leads use measured block/panel centres, dimensions and short-gap elbows", (t) => {
  const h = panelHarness(t);
  h.add("a", "milestone", 99000);
  h.layout();
  const panel = h.panels()[0];
  assert.equal(h.svg.parentNode, h.group);
  assert.equal(panel.contains(h.svg), false);
  assert.equal(h.svg.getAttribute("width"), "1000");
  assert.equal(h.svg.getAttribute("height"), "130");
  assert.equal(h.svg.firstChild.getAttribute("class"), "milestone");
  assert.equal(h.svg.firstChild.getAttribute("d"), "M 704.0 64.0 L 704.0 72.0 L 704.0 72.0 L 704.0 80.0");
  // The rendered position can be mid-transition, not the target style.left.
  panel.dataset.visualLeft = "400";
  panel.dataset.top = "170";
  h.run("drawTimelineLeads()");
  assert.equal(h.svg.firstChild.getAttribute("d"), "M 704.0 64.0 L 704.0 67.0 L 490.0 67.0 L 490.0 70.0");
  h.run("animateTimelineLeads()");
  panel.dataset.visualLeft = "420";
  h.frame(200);
  assert.match(h.svg.firstChild.getAttribute("d"), /L 510\.0 70\.0$/);
  assert.equal(h.frames.length, 1);
  h.frame(481);
  assert.equal(h.frames.length, 0, "animation stops after the slide completes");
});

test("project rows appear and disappear with the visible time window", async () => {
  const errors = [];
  const virtualConsole = new VirtualConsole();
  virtualConsole.on("jsdomError", (error) => errors.push(error.message));
  const dom = new JSDOM(fs.readFileSync(`${__dirname}/index.html`, "utf8"), {
    url: "https://dashboard.test",
    runScripts: "dangerously",
    pretendToBeVisual: true,
    virtualConsole,
    beforeParse(w) {
      w.fetch = async () => ({ ok: true, status: 200, json: async () => [] });
      w.EventSource = class {
        addEventListener() {}
      };
      w.scrollTo = () => {};
    },
  });
  try {
    await new Promise((resolve) => setTimeout(resolve, 30));
    const w = dom.window;
    const now = Date.now();
    w.Date.now = () => now;
    w.eval(`
      data.workers = ['recent', 'historic', 'empty'].map(slug => ({slug, name: slug}));
      data.activities = [
        {id:'r', slug:'recent', kind:'note', text:'recent', ts:${now - 60000}},
        {id:'h', slug:'historic', kind:'progress', text:'historic', ts:${now - 3600000}}
      ];
      loaded.add('workers'); loaded.add('activities'); renderProjects();
    `);
    const scroller = w.document.querySelector("#timelineScroll");
    Object.defineProperty(scroller, "clientWidth", { value: 900 });
    Object.defineProperty(scroller, "scrollWidth", { value: 8820 });
    w.eval("updateTimeline()");
    const visible = () =>
      [...w.document.querySelectorAll(".timeline-group")]
        .filter((row) => !row.classList.contains("collapsed"))
        .map((row) => row.querySelector(".timeline-track").dataset.slug);
    const collapsed = () =>
      w.document.querySelectorAll(".timeline-group.collapsed").length;
    const scroll = (left) => {
      scroller.scrollLeft = left;
      scroller.dispatchEvent(new w.Event("scroll"));
    };
    assert.deepEqual(visible(), ["recent"]);
    scroll(5400); // Window around one hour ago; no timer tick required.
    assert.deepEqual(visible(), ["historic"]);
    assert.equal(collapsed(), 2, "recent and always-empty rows collapse");
    scroll(1000);
    assert.deepEqual(visible(), []);
    assert.equal(collapsed(), 3, "all rows collapse in an empty window");
    assert.equal(w.document.querySelector("#timelineEmpty").hidden, false);
    assert(scroller.isConnected, "empty windows retain history navigation");
    scroll(7920);
    assert.deepEqual(visible(), ["recent"]);
    assert.equal(w.document.querySelector("#timelineEmpty").hidden, true);
    // Clock advancement removes the last item without an SSE re-render.
    w.Date.now = () => now + 16 * 60000;
    w.eval("updateTimeline()");
    assert.deepEqual(visible(), []);
    assert.deepEqual(errors, []);
  } finally {
    dom.window.close();
  }
});
