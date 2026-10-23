// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/attention.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");

const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
const at = (marker) => {
  const index = script.indexOf(marker);
  assert.notEqual(index, -1, `marker not found: ${marker}`);
  return index;
};

// askCard + renderAttention, plus only the helpers they call.
const attentionHelpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function button("), at("function facts(")],
  [at("function empty("), at("function record(")],
  [at("function replaceContent("), at("function count(")],
  [at("function askCard("), at("function renderTraffic()")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

const taskWait = (extra) => ({
  kind: "task_wait",
  id: "wait-1",
  taskId: "task-1",
  from: "ship",
  title: "Write the release notes",
  question: "Ready to accept?",
  reason: "review",
  ...extra,
});

function attentionHarness(t, asks) {
  const dom = new JSDOM(`<div id="content"></div>`, { url: "http://piso.local/" });
  t.after(() => dom.window.close());
  const { window } = dom;
  const sandbox = {
    document: window.document,
    window,
    $: (selector) => window.document.querySelector(selector),
    data: { asks, plans: [], mcpPending: [], githubPending: [] },
    loaded: new Set(["asks", "plans", "mcpPending", "githubPending"]),
    list: (key) => sandbox.data[key] || [],
    routeURL: (name) => `http://${name}.piso.local`,
    ingressPort: 80,
  };
  const context = new Proxy(sandbox, {
    has: () => true,
    get: (target, key) => (key in target ? target[key] : globalThis[key]),
  });
  vm.createContext(context);
  vm.runInContext(`"use strict";\n${attentionHelpers}\nrenderAttention();`, context);
  return window.document;
}

test("review wait renders accept/request-changes/cancel actions", (t) => {
  const document = attentionHarness(t, [taskWait()]);
  const card = document.querySelector(".ask");
  assert.ok(card, "a card is rendered");
  assert.match(card.textContent, /Ready for review/);
  assert.match(card.textContent, /Write the release notes/);
  const buttons = [...card.querySelectorAll("button")];
  assert.deepEqual(
    buttons.map((b) => b.dataset.action),
    ["ask-accept", "ask-request_changes", "ask-cancel"],
  );
  assert.equal(buttons[0].dataset.id, "wait-1");
});

test("needs_input wait renders resolve/cancel actions", (t) => {
  const document = attentionHarness(t, [taskWait({ reason: "needs_input" })]);
  const card = document.querySelector(".ask");
  assert.match(card.textContent, /Needs input/);
  assert.deepEqual(
    [...card.querySelectorAll("button")].map((b) => b.dataset.action),
    ["ask-resolve", "ask-cancel"],
  );
});

test("non-task asks still render decision cards", (t) => {
  const document = attentionHarness(t, [
    { kind: "approval", id: "a-1", from: "gw", title: "Publish", action: "publish" },
  ]);
  const card = document.querySelector(".ask");
  assert.match(card.textContent, /Publish/);
  assert.deepEqual(
    [...card.querySelectorAll("button")].map((b) => b.dataset.action),
    ["ask-approved", "ask-rejected"],
  );
});

// The dispatcher's ask- branch posts task_wait decisions to the respond route.
const dispatcherSlice = script.slice(
  at('if (name.startsWith("ask-")) {'),
  at('if (name === "mcp-auth") return authorizeMCP(id);'),
);

// ``source`` is the clicked button and ``notify`` is the toast: the real action()
// takes all three, so the harness must declare them or the slice cannot run.
function dispatcher(asks, promptValue = "looks good", options = {}) {
  const calls = [];
  const toasts = [];
  const refreshes = [];
  const window = { prompt: () => promptValue };
  const list = (key) => (key === "asks" ? asks : []);
  const api = async (...args) => {
    calls.push(args);
    if (options.fail) throw options.fail;
    return {};
  };
  const factory = new Function(
    "window",
    "list",
    "api",
    "enc",
    "rid",
    "changed",
    "notify",
    "source",
    `return async function action(name, id) {\n${dispatcherSlice}\n};`,
  );
  const action = factory(
    window,
    list,
    api,
    encodeURIComponent,
    () => "req-1",
    async (keys) => refreshes.push(keys),
    (text) => toasts.push(text),
    options.source,
  );
  return { action, calls, toasts, refreshes };
}

// A click target shaped like the rendered card: the revision the human is
// looking at lives in the DOM, not in whatever the last poll put in memory.
function cardButton(revision) {
  const card = { dataset: revision === undefined ? {} : { revision } };
  return { closest: (sel) => (sel === ".ask" ? card : null) };
}

test("respond dispatcher posts the idempotency key and wait id", async () => {
  const { action, calls } = dispatcher([taskWait()]);
  await action("ask-accept", "wait-1");
  assert.equal(calls.length, 1);
  const [path, method, body] = calls[0];
  assert.equal(path, "tasks/task-1/respond");
  assert.equal(method, "POST");
  assert.deepEqual(body, {
    action: "accept",
    summary: "looks good",
    waitId: "wait-1",
    requestId: "req-1",
  });
});

test("respond dispatcher aborts when the prompt is cancelled", async () => {
  const { action, calls } = dispatcher([taskWait({ reason: "needs_input" })], null);
  await action("ask-resolve", "wait-1");
  assert.equal(calls.length, 0);
});

test("respond dispatcher routes approvals to the decide endpoint", async () => {
  const { action, calls } = dispatcher([
    { kind: "approval", id: "a-1", from: "gw", title: "Publish" },
  ]);
  await action("ask-approved", "a-1");
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], "asks/approvals/a-1/decide");
  assert.deepEqual(calls[0][2], { decision: "approved" });
});

// A proposed task's card is the one ask whose text can move under the reader, so
// the decision must name the revision that was rendered, and a stale one must
// refresh instead of failing at the human.
test("proposal decision cites the revision rendered on the card", async () => {
  const { action, calls, toasts } = dispatcher(
    [{ kind: "approval", id: "a-1", action: "task.propose", title: "Add a host", revision: "R2" }],
    "unused",
    { source: cardButton("R1") },
  );
  await action("ask-approved", "a-1");
  assert.equal(calls.length, 1);
  // The card said R1 while memory already held R2: the human read R1.
  assert.deepEqual(calls[0][2], { decision: "approved", seenRevision: "R1" });
  assert.deepEqual(toasts, []);
});

test("a stale proposal refreshes the card and explains instead of erroring", async () => {
  const stale = Object.assign(new Error("proposal was revised after you loaded it"), {
    status: 409,
  });
  const { action, calls, toasts, refreshes } = dispatcher(
    [{ kind: "approval", id: "a-1", action: "task.propose", title: "Add a host", revision: "R2" }],
    "unused",
    { source: cardButton("R1"), fail: stale },
  );
  // Resolving means the human was told and the card was refetched, not that an
  // error dialog appeared.
  await assert.doesNotReject(() => action("ask-approved", "a-1"));
  assert.deepEqual(refreshes, [["asks"]], "the card was refetched");
  assert.equal(calls.length, 1, "the decision was posted once, not retried");
  assert.match(toasts[0], /check it and decide again/);
});

test("other approvals decide without a revision", async () => {
  const { action, calls, toasts } = dispatcher([
    { kind: "approval", id: "a-1", from: "gw", title: "Publish", action: "publish" },
  ]);
  await action("ask-approved", "a-1");
  assert.deepEqual(calls[0][2], { decision: "approved" });
  assert.deepEqual(toasts, []);
});
