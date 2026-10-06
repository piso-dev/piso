// Run with: node --test gateway/internal/server/ui/timeline.test.cjs
// No browser dependencies: test the production timeline math and time boundaries.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
function harness() {
  const context = vm.createContext({
    window: { addEventListener() {} },
    setInterval() {},
  });
  vm.runInContext(
    script.slice(
      script.indexOf("      const TL_HOUR"),
      script.indexOf("      function askCard"),
    ),
    context,
  );
  return (code) =>
    JSON.parse(vm.runInContext(`JSON.stringify(${code})`, context));
}
test("embedded dashboard JavaScript parses", () => new vm.Script(script));
test("merges only consecutive same-kind events up to three minutes apart", () => {
  const run = harness();
  const blocks = run(`mergeTimelineEvents([
    {id:'a',ts:500000,kind:'progress'},
    {id:'beat',ts:510000,kind:'progress'},
    {id:'b',ts:680000,kind:'progress'},
    {id:'c',ts:860001,kind:'progress'},
    {id:'d',ts:870000,kind:'milestone'}
  ], 1000000)`);
  assert.equal(blocks.length, 3);
  assert.deepEqual(
    blocks[0].events.map((e) => e.id),
    ["a", "beat", "b"],
  );
  assert.equal(blocks[0].start, 500000);
  assert.equal(blocks[0].end, 680000);
});
test("three-hour cutoff excludes older, future and invalid timestamps; respects projected heartbeats", () => {
  const run = harness();
  const blocks = run(`mergeTimelineEvents([
    {ts:30000000-TL_HISTORY-1,kind:'note'},
    {ts:30000000-TL_HISTORY,kind:'note'},
    {ts:30000001,kind:'note'},
    {ts:'invalid',kind:'note'},
    {ts:29999999,activeFromMs:29900000,kind:'active'}
  ],30000000)`);
  assert.equal(blocks.length, 2);
  assert.equal(blocks[1].start, 29900000);
});
test("newest callout stays at the leading edge and pushes older panels left without gaps", () => {
  const run = harness();
  const layout = run(
    `layoutTimelineCallouts([{ts:1000},{ts:999},{ts:998}], t=>t, 260)`,
  );
  assert.deepEqual(
    layout.map((p) => p.left),
    [740, 480, 220],
  );
});
test("callouts expire at 60 seconds and never include heartbeat or future events", () => {
  const run = harness();
  assert.deepEqual(
    run(`recentTimelineEvents([
    {id:'expired',ts:40000,kind:'note'},
    {id:'fresh',ts:40001,kind:'note'},
    {id:'beat',ts:100000,kind:'active'},
    {id:'future',ts:100001,kind:'note'}
  ],100000).map(e=>e.id)`),
    ["fresh"],
  );
});
test("returns to now at 30 seconds but not during a pointer drag", () => {
  const run = harness();
  assert.equal(
    run(
      `(()=>{timeline.following=false;timeline.lastInteraction=1000;timeline.end=1000;timelineFollow(30999);return timeline.end})()`,
    ),
    1000,
  );
  assert.equal(
    run(`(()=>{timelineFollow(31000);return timeline.end})()`),
    31000,
  );
  assert.equal(
    run(
      `(()=>{timeline.following=false;timeline.pointerDown=true;timelineFollow(40000);return timeline.following})()`,
    ),
    false,
  );
});
test("scrolling to the right edge immediately pins to now and keeps following", () => {
  const handlers = {};
  const scroller = {
    scrollWidth: 8820,
    clientWidth: 900,
    scrollLeft: 100,
    addEventListener: (type, handler) => {
      handlers[type] = handler;
    },
  };
  const context = vm.createContext({
    window: { addEventListener() {} },
    setInterval() {},
    scroller,
  });
  vm.runInContext(
    script.slice(
      script.indexOf("      const TL_HOUR"),
      script.indexOf("      function askCard"),
    ),
    context,
  );
  // Capture production wheel/scroll listeners without constructing the page DOM.
  const start = script.indexOf(
    "        scroller.addEventListener(",
    script.indexOf("      function mountTimeline()"),
  );
  const end = script.indexOf(
    '        scroller.addEventListener("pointerover"',
    start,
  );
  assert(start >= 0 && end > start, "production scroll handler exists");
  vm.runInContext(
    `
    let testNow = 60000;
    timelineInteract = () => { timeline.following = false; timeline.lastInteraction = testNow; };
    hideTimelineTip = () => { timeline.tipKey = null; };
    updateTimeline = () => {
      timelineFollow(testNow);
      if (timeline.following) scroller.scrollLeft = scroller.scrollWidth - scroller.clientWidth;
    };
    timeline.following = false;
    timeline.end = 1000;
    timeline.expectedScroll = 100;
    timeline.lastInteraction = testNow;
    timeline.pointerDown = true;
    timeline.tipKey = 'block:alpha:event';
  `,
    context,
  );
  vm.runInContext(script.slice(start, end), context);
  scroller.scrollLeft = 7920 - 24.5;
  handlers.scroll();
  assert.equal(vm.runInContext("timeline.following", context), false);
  scroller.scrollLeft = 7920 - 24;
  handlers.scroll();
  assert.equal(vm.runInContext("timeline.following", context), true);
  assert.equal(scroller.scrollLeft, 7920, "snap fully to the edge");
  assert.equal(vm.runInContext("timeline.end", context), 60000);
  assert.equal(vm.runInContext("timeline.tipKey", context), null);
  vm.runInContext("testNow += 1000; updateTimeline()", context);
  assert.equal(vm.runInContext("timeline.end", context), 61000);
  // Momentum at the hard edge produces wheel events without scroll events.
  for (let i = 0; i < 3; i++) {
    handlers.wheel({ deltaX: 10, deltaY: 0 });
    assert.equal(vm.runInContext("timeline.following", context), true);
  }
  handlers.wheel({ deltaX: 0, deltaY: 10, shiftKey: true });
  assert.equal(vm.runInContext("timeline.following", context), true);
  handlers.wheel({ deltaX: -10, deltaY: 0 });
  assert.equal(vm.runInContext("timeline.following", context), false);
  scroller.scrollLeft = 100;
  handlers.scroll();
  assert.equal(vm.runInContext("timeline.following", context), false);
});
test("timeline offsets apply to absolutely positioned blocks and panels", () => {
  const css = html.match(/<style>([\s\S]*?)<\/style>/)[1];
  for (const selector of [
    "timeline-block",
    "timeline-callout",
    "timeline-live-line",
  ]) {
    assert.match(
      css,
      new RegExp(`\\.${selector}\\s*\\{[^}]*position:\\s*absolute\\s*;`),
    );
  }
  assert.match(css, /\.timeline-track\s*\{[^}]*position:\s*relative\s*;/);
});
test("fixed lanes leave the top 20% exposed and callouts stay passive", () => {
  assert.match(html, /\.timeline-row\s*\{[^}]*height:\s*100px/);
  assert.match(
    html,
    /\.timeline-callout\s*\{[^}]*bottom:\s*0;[^}]*height:\s*80%;[^}]*z-index:\s*2;/,
  );
  assert.match(
    html,
    /\.timeline-tip\s*\{[^}]*position:\s*fixed;[^}]*z-index:\s*20;/,
  );
  assert.doesNotMatch(html, /Activity & context|Signal strips/);
  assert.doesNotMatch(
    html,
    /class="[^"]*timeline-callout[^"]*"[^>]*data-timeline-key/,
  );
});
test("block layout keeps events visible and lets later events overdraw", () => {
  const run = harness();
  const layout = run(
    `timelineBlockLayout([
    {start:0,end:300000},
    {start:300000,end:305000},
    {start:310000,end:600000}
  ], t=>t/1000, 700)`,
  );
  assert.deepEqual(layout, [
    { left: 0, width: 295 },
    { left: 297, width: 8 },
    { left: 310, width: 290 },
  ]);
});
test("point events render right-anchored like the previous board, 8px wide with 2px gaps", () => {
  const run = harness();
  const layout = run(
    `timelineBlockLayout([
    {start:700,end:700},
    {start:702,end:702},
    {start:800,end:800}
  ], t=>t, 2000)`,
  );
  assert.deepEqual(layout, [
    { left: 684, width: 8 },
    { left: 694, width: 8 },
    { left: 792, width: 8 },
  ]);
  const liveEdge = run(
    `timelineBlockLayout([{start:1000,end:1000}], t=>t, 1000)`,
  );
  assert.deepEqual(liveEdge, [{ left: 992, width: 8 }]);
});
