// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/worker-scope.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

const at = (marker) => {
  const index = script.indexOf(marker);
  assert.notEqual(index, -1, `Missing script marker: ${marker}`);
  return index;
};
const helpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function field("), at("function formDialog(")],
  [at("function button("), at("function facts(")],
  [at("function notify("), at("function mountDialog(")],
  [at("function mountDialog("), at("function closeDialog(")],
  [at("function closeDialog("), at("function field(")],
  [at("function formDialog("), at("function confirmAction(")],
  [at("function secretForm("), at("function workerScopeField(")],
  [at("function workerScopeField("), at("function providerForm(")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

const WORKERS = [
  { slug: "alpha", name: "piso-worker-alpha" },
  { slug: "beta", name: "piso-worker-beta" },
  { slug: "gamma", name: "piso-worker-gamma" },
];

function scopeHarness(t, secret = {}, workers = WORKERS) {
  // Scope tests need a valid host binding; unbound worker secrets are rejected.
  secret = { allowedHosts: ["api.example.test"], ...secret };
  const dom = new JSDOM(
    `<dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog><div id="toast" hidden></div>`,
    { url: "http://piso.local/" },
  );
  t.after(() => dom.window.close());
  const { window } = dom;
  const calls = [];
  const sandbox = {
    document: window.document,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    Event: window.Event,
    FormData: window.FormData,
    api: async (path, method, body) => {
      calls.push([path, method, JSON.parse(JSON.stringify(body))]);
      return { secret: body };
    },
    changed: async () => {},
    dialogBusy: false,
    dialogReturn: null,
    toastTimer: null,
    data: { workers, secrets: [secret], provider: {} },
    list: (key) => sandbox.data[key] || [],
  };
  window.HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
  };
  window.HTMLDialogElement.prototype.close = function () {
    this.open = false;
  };
  sandbox.$ = (s) => window.document.querySelector(s);
  vm.createContext(sandbox);
  vm.runInContext(`"use strict";\n${helpers}\nsecretForm(${JSON.stringify(secret.id || "")});`, sandbox);
  if (!secret.id) {
    window.document.querySelector("#f-name").value = "test secret";
    window.document.querySelector("#f-placeholder").value = "piso_test_secret";
    window.document.querySelector("#f-value").value = "test-value";
    window.document.querySelector("#f-allowedHosts").value = "api.example.test";
  }
  return { window, calls, dialog: window.document.querySelector("dialog") };
}

test("add secret shows the triple scope control with All workers active", (t) => {
  const { dialog } = scopeHarness(t);
  const options = [...dialog.querySelectorAll(".scope-option")];
  assert.deepEqual(
    options.map((b) => b.dataset.scope),
    ["none", "all", "some"],
  );
  assert.equal(
    dialog.querySelector('.scope-option[data-scope="all"]').classList.contains("active"),
    true,
    "legacy default is All workers",
  );
  assert.equal(dialog.querySelector("#workerPick").hidden, true);
});

test("Select workers reveals the picker with every toggle switched on", (t) => {
  const { window, dialog } = scopeHarness(t);
  dialog.querySelector('.scope-option[data-scope="some"]').click();
  assert.equal(dialog.querySelector("#workerPick").hidden, false);
  const toggles = [...dialog.querySelectorAll(".worker-toggle")];
  assert.equal(toggles.length, 3);
  assert.deepEqual(
    toggles.map((b) => b.dataset.on),
    ["1", "1", "1"],
    "defaults switched on",
  );
  toggles.forEach((b) => assert.equal(b.classList.contains("on"), true));
  // toggling off works
  toggles[0].click();
  assert.equal(toggles[0].dataset.on, "0");
  assert.equal(toggles[0].textContent.trim(), "Off");
  assert.equal(toggles[1].dataset.on, "1");
});

test("filter narrows rows and saving persists the selected slugs", async (t) => {
  const { window, calls, dialog } = scopeHarness(t);
  dialog.querySelector('.scope-option[data-scope="some"]').click();
  const search = dialog.querySelector("#workerSearch");
  search.value = "bet";
  search.dispatchEvent(new window.Event("input"));
  const rows = [...dialog.querySelectorAll("#workerList .worker-row")];
  assert.deepEqual(
    rows.map((r) => (r.hidden ? "hidden" : "visible")),
    ["hidden", "visible", "hidden"],
    "filter matches worker names case-insensitively",
  );
  search.value = "";
  search.dispatchEvent(new window.Event("input"));
  dialog.querySelector('.worker-toggle[data-slug="alpha"]').click();
  dialog.querySelector('.worker-toggle[data-slug="gamma"]').click();
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(calls.length, 1);
  const [, method, body] = calls[0];
  assert.equal(method, "POST");
  assert.equal(body.gatewayOnly, false);
  assert.deepEqual(body.workers, ["beta"]);
});

test("saving with Select workers and nothing enabled is rejected client-side", async (t) => {
  const { calls, dialog } = scopeHarness(t);
  dialog.querySelector('.scope-option[data-scope="some"]').click();
  [...dialog.querySelectorAll(".worker-toggle")].forEach((b) => {
    if (b.dataset.on === "1") b.click();
  });
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(calls.length, 0, "no API call when the selection is empty");
  const err = dialog.querySelector("#formError");
  assert.equal(err.hidden, false);
  assert.match(err.textContent, /Select at least one worker/);
});

test("No workers saves a gateway-only secret", async (t) => {
  const { calls, dialog } = scopeHarness(t);
  dialog.querySelector('.scope-option[data-scope="none"]').click();
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(calls.length, 1);
  const [, , body] = calls[0];
  assert.equal(body.gatewayOnly, true);
  assert.deepEqual(body.workers, []);
});

test("editing a scoped secret preselects only its workers and saves the narrowed set", async (t) => {
  const secret = {
    id: "sec-1",
    name: "scoped",
    placeholder: "piso_scoped",
    workers: ["beta"],
  };
  const { calls, dialog } = scopeHarness(t, secret);
  assert.equal(
    dialog.querySelector('.scope-option[data-scope="some"]').classList.contains("active"),
    true,
    "existing narrow scope opens in Select workers mode",
  );
  assert.deepEqual(
    [...dialog.querySelectorAll(".worker-toggle")].map((b) => b.dataset.on),
    ["0", "1", "0"],
    "only the saved worker is preselected",
  );
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  const [, method, body] = calls[0];
  assert.equal(method, "PUT");
  assert.deepEqual(body.workers, ["beta"]);
});

test("empty worker registry shows an empty state and cannot grant all by accident", async (t) => {
  const { calls, dialog } = scopeHarness(t, {}, []);
  dialog.querySelector('[data-scope="some"]').click();
  assert.match(dialog.querySelector("#workerEmpty").textContent, /No named workers/);
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(calls.length, 0);
  assert.match(dialog.querySelector("#formError").textContent, /Select at least one worker/);
});

test("unregistered saved workers survive editing", async (t) => {
  const { calls, dialog } = scopeHarness(t, {
    id: "sec-old", name: "old", workers: ["retired"],
  });
  assert.equal(dialog.querySelector('[data-slug="retired"]').dataset.on, "1");
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(calls[0][2].workers, ["retired"]);
});

test("wildcard scopes open on All workers", (t) => {
  const { dialog } = scopeHarness(t, { id: "sec-star", name: "star", workers: ["*"] });
  assert.equal(dialog.querySelector(".scope-option.active").dataset.scope, "all");
  assert.equal(dialog.querySelector('[data-slug="*"]'), null);
});

test("switching a scoped secret to All workers clears restrictions", async (t) => {
  const { calls, dialog } = scopeHarness(t, { id: "sec-all", name: "all", workers: ["beta"] });
  dialog.querySelector('[data-scope="all"]').click();
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(calls[0][2].workers, []);
  assert.equal(calls[0][2].gatewayOnly, false);
});

test("editing a gateway-only secret opens on No workers and keeps it on save", async (t) => {
  const secret = {
    id: "sec-2",
    name: "vault",
    placeholder: "piso_vault",
    gatewayOnly: true,
  };
  const { calls, dialog } = scopeHarness(t, secret);
  assert.equal(
    dialog.querySelector('.scope-option[data-scope="none"]').classList.contains("active"),
    true,
  );
  dialog.querySelector('button[type="submit"]').click();
  await new Promise((r) => setTimeout(r, 10));
  const [, , body] = calls[0];
  assert.equal(body.gatewayOnly, true);
  assert.deepEqual(body.workers, []);
});
