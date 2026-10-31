// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/provider-form.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

// The page script is one large top-level scope, so slice out just the helpers
// the provider dialog needs and run them against a stubbed page.
const at = (marker) => script.indexOf(marker);
const helpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function age("), at("function badge(")],
  [at("function field("), at("function formDialog(")],
  [at("function button("), at("function facts(")],
  [at("function notify("), at("function mountDialog(")],
  [at("function mountDialog("), at("function closeDialog(")],
  [at("function closeDialog("), at("function field(")],
  [at("function formDialog("), at("function confirmAction(")],
  // providerDriftLines renders inside a card, which lives outside the
  // card/dialog pair this slice used to cover.
  [at("function providerDriftLines("), at("function providerCard(")],
  [at("function providerCard("), at("function providerForm(")],
  // providerCard..providerForm already carries the override dialog and the
  // combobox/chip helpers it wires, so only the tail is still needed here.
  [at("function providerForm("), at("function addPolicy(")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

function formHarness(t, { catalog, fail = false, baseUrl = "https://api.example.test/v1", provider = {}, formId = "p-1" } = {}) {
  const dom = new JSDOM(`<dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog><div id="toast" hidden></div>`, {
    url: "http://piso.local/",
  });
  t.after(() => dom.window.close());
  const { window } = dom;
  const calls = [];
  const payloads = [];
  const sandbox = {
    document: window.document,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    KeyboardEvent: window.KeyboardEvent,
    Event: window.Event,
    // formDialog reads the dialog's fields through FormData, so a submit test
    // needs the page's own constructor.
    FormData: window.FormData,
    api: async (path, method, body) => {
      calls.push(path);
      payloads.push({ path, method, body });
      if (fail) throw new Error("provider model catalog is unavailable");
      return catalog;
    },
    dialogBusy: false,
    dialogReturn: null,
    data: {
      provider: {
        providers: [
          { id: "p-1", name: "AIML", secretId: "sec-1", baseUrl, defaultModel: "model-b", ...provider },
        ],
        activeId: "p-1",
      },
      secrets: [{ id: "sec-1", name: "AIML", technocoreId: "sec-1" }],
    },
    list: (key) => sandbox.data[key] || [],
    changed: async () => {},
  };
  window.HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
  };
  sandbox.$ = (s) => window.document.querySelector(s);
  vm.createContext(sandbox);
  vm.runInContext(`"use strict";\n${helpers}\nproviderForm(${JSON.stringify(formId)});`, sandbox);
  return { window, calls, payloads, dialog: window.document.querySelector("dialog") };
}

// The models request always carries the live form values so an unsaved
// provider can load its catalog too.
function assertCatalogCalls(calls, { id, baseUrl, secretId }) {
  assert.equal(calls.length, 1);
  assert.match(calls[0], /^pi-provider\/models\?/);
  assert.match(calls[0], new RegExp("id=" + (id || "") + "(?:&|$)"));
  assert.match(calls[0], new RegExp("baseUrl=" + encodeURIComponent(baseUrl)));
  assert.match(calls[0], new RegExp("secretId=" + encodeURIComponent(secretId)));
}

test("default model field suggests catalog matches and keeps the saved value", async (t) => {
  const { window, calls, dialog } = formHarness(t, {
    catalog: { models: ["model-a", "model-b", "other-model"], catalog: "ok" },
  });
  const input = dialog.querySelector("#f-defaultModel");
  const list = dialog.querySelector("#modelList");
  assert.equal(input.value, "model-b");
  assert.equal(input.getAttribute("role"), "combobox");
  assert.equal(input.getAttribute("aria-controls"), "modelList");
  await new Promise((r) => setTimeout(r, 20));
  assertCatalogCalls(calls, {
    id: "p-1",
    baseUrl: "https://api.example.test/v1",
    secretId: "sec-1",
  });
  assert.match(dialog.querySelector("#modelHint").textContent, /3 models available/);

  input.value = "model";
  input.dispatchEvent(new window.Event("input"));
  assert.deepEqual(
    [...list.querySelectorAll("li")].map((li) => li.dataset.model),
    ["model-a", "model-b", "other-model"],
  );
  assert.equal(list.hidden, false);

  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "ArrowDown" }));
  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter" }));
  assert.equal(input.value, "model-a");
  assert.equal(list.hidden, true);

  input.value = "zzz";
  input.dispatchEvent(new window.Event("input"));
  assert.equal(list.hidden, true, "no matches hides the list");
});

test("a catalog that cannot be loaded leaves the field as plain text", async (t) => {
  const { dialog } = formHarness(t, { fail: true });
  await new Promise((r) => setTimeout(r, 20));
  const hint = dialog.querySelector("#modelHint").textContent;
  assert.match(hint, /unavailable/);
  assert.doesNotMatch(hint, /502|Error/);
  assert.equal(dialog.querySelector("#modelList").hidden, true);
  assert.equal(dialog.querySelector("#f-defaultModel").value, "model-b");
});

test("a failed catalog is reported without blocking the saved value", async (t) => {
  const { dialog } = formHarness(t, {
    catalog: { models: ["cached-model"], catalog: "failed", error: "unavailable" },
  });
  await new Promise((r) => setTimeout(r, 20));
  const hint = dialog.querySelector("#modelHint").textContent;
  assert.match(hint, /unavailable/);
  assert.match(hint, /Type a model id/);
  assert.equal(dialog.querySelector("#f-defaultModel").value, "model-b");
});

test("an unsaved provider change does not offer the previous catalog", async (t) => {
  const { calls, dialog } = formHarness(t, {
    catalog: { models: ["model-a"] },
    baseUrl: "",
  });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(calls.length, 0);
  // The secret, not the URL, is what unlocks a catalog now, so the hint asks
  // for one. A provider with no URL still offers nothing from a previous save.
  assert.match(dialog.querySelector("#modelHint").textContent, /Choose an API key secret/);
  assert.equal(dialog.querySelector("#modelList").hidden, true);
});

test("the add modal loads the catalog as soon as the URL and secret are set", async (t) => {
  const { window, calls, dialog } = formHarness(t, {
    catalog: { models: ["model-a", "model-b"], catalog: "ok" },
    formId: "",
  });
  const baseUrl = dialog.querySelector("#f-baseUrl");
  const secret = dialog.querySelector("#f-secretId");
  baseUrl.value = "https://api.example.test/v1";
  secret.value = "sec-1";
  baseUrl.dispatchEvent(new window.Event("input"));
  assert.equal(calls.length, 0, "no request before the debounce elapses");
  await new Promise((r) => setTimeout(r, 320));
  assertCatalogCalls(calls, {
    id: "",
    baseUrl: "https://api.example.test/v1",
    secretId: "sec-1",
  });
  assert.match(dialog.querySelector("#modelHint").textContent, /2 models available/);
});

test("changing the URL discards the previous catalog and reloads", async (t) => {
  const { window, calls, dialog } = formHarness(t, {
    catalog: { models: ["model-a"], catalog: "ok" },
  });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(calls.length, 1);
  const baseUrl = dialog.querySelector("#f-baseUrl");
  baseUrl.value = "https://other.example.test/v1";
  baseUrl.dispatchEvent(new window.Event("input"));
  await new Promise((r) => setTimeout(r, 320));
  assert.equal(calls.length, 2);
  assert.match(calls[1], /baseUrl=https%3A%2F%2Fother\.example\.test%2Fv1/);
});

// Provider cards render the catalog text straight from the API view; a
// checked-but-empty catalog is not the same as "pending".
function cardText(t, provider) {
  const dom = new JSDOM(`<dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog><div id="toast" hidden></div>`, { url: "http://piso.local/" });
  t.after(() => dom.window.close());
  const sandbox = {
    document: dom.window.document,
    data: { provider: { providers: [] } },
    $: (s) => dom.window.document.querySelector(s),
  };
  vm.createContext(sandbox);
  return vm.runInContext(
    `"use strict";\n${helpers}\nproviderCard(${JSON.stringify(provider)});`,
    sandbox,
  );
}

test("provider cards report the loaded catalog instead of pending", (t) => {
  const withModels = cardText(t, { id: "p-1", catalog: "ok", models: ["model-a", "model-b"] });
  assert.match(withModels, /provider-status/);
  assert.match(withModels, /2 models/);
  assert.doesNotMatch(withModels, /pending/);

  const empty = cardText(t, { id: "p-2", catalog: "ok", models: [] });
  assert.match(empty, /no models listed/);
  assert.doesNotMatch(empty, /pending/);

  const failed = cardText(t, { id: "p-3", catalog: "failed" });
  assert.match(failed, /model list unavailable/);
});

// The card view asks the gateway for the same catalog the edit dialog loads,
// once per provider, so the two never disagree.
test("provider cards load the same catalog the edit dialog would", async (t) => {
  const dom = new JSDOM(`<div id="grid"></div>`);
  t.after(() => dom.window.close());
  const calls = [];
  const providers = [{ id: "p-1", catalog: "pending" }];
  const sandbox = {
    document: dom.window.document,
    data: { provider: { providers } },
    view: "access",
    sub: "provider",
    providerCatalogs: new Set(),
    api: async (path) => {
      calls.push(path);
      return { models: ["model-a", "model-b"], catalog: "ok", checkedAt: "2026-01-01T00:00:00Z" };
    },
    // A finished refresh asks for the provider list once, because only the
    // gateway can say which models the new catalog left missing.
    changed: async (keys) => calls.push("changed:" + keys.join(",")),
  };
  vm.createContext(sandbox);
  // The refresh flag is module state the loader reads, so it is carried in
  // rather than sliced: it lives far above the function under test.
  const loader =
    "const catalogRefresh = { inFlight: false, settled: false };\n" +
    script.slice(at("function loadProviderCatalogs("), at("function addPolicy("));
  vm.runInContext(`"use strict";\n${loader}\nloadProviderCatalogs();`, sandbox);
  await new Promise((r) => setTimeout(r, 20));
  assert.deepEqual(calls, ["pi-provider/models?id=p-1", "changed:provider"]);
  assert.equal(providers[0].catalog, "ok");
  assert.deepEqual(providers[0].models, ["model-a", "model-b"]);
  assert.equal(providers[0].checkedAt, "2026-01-01T00:00:00Z");
  vm.runInContext("loadProviderCatalogs();", sandbox);
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(calls, ["pi-provider/models?id=p-1", "changed:provider"], "a provider catalog is fetched once per page");
});

test("model ids from the catalog are escaped in the suggestion list", async (t) => {
  const { window, dialog } = formHarness(t, { catalog: { models: ['<img src=x onerror=alert(1)>'], catalog: "ok" } });
  await new Promise((r) => setTimeout(r, 20));
  const input = dialog.querySelector("#f-defaultModel");
  input.value = "<";
  input.dispatchEvent(new window.Event("input"));
  const item = dialog.querySelector("#modelList li");
  assert.equal(item.querySelector("img"), null);
  assert.equal(item.textContent, "<img src=x onerror=alert(1)>");
});

// Approved models are edited as chips: each id is removable on its own, and the
// value the form submits is still the newline-joined list the API takes.
test("approved models are chips that add from the catalog and remove on x", async (t) => {
  const { window, dialog } = formHarness(
    t,
    { catalog: { models: ["model-a", "model-b", "model-c"], catalog: "ok" }, provider: { approvedModels: ["model-b"] } },
  );
  await new Promise((r) => setTimeout(r, 20));
  const holder = dialog.querySelector("#f-approvedModels-chips");
  const input = dialog.querySelector("#f-approvedModels-input");
  const store = dialog.querySelector("#f-approvedModels");
  // The saved list is what the dialog opens on, and it is what a save resends.
  assert.deepEqual(
    [...holder.querySelectorAll(".chip-id")].map((c) => c.textContent),
    ["model-b"],
  );
  assert.equal(store.value, "model-b");

  // Typing an id and pressing Enter adds it, and the input is cleared for the
  // next one so a chip list can be built without touching the mouse.
  input.value = "model-a";
  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  assert.deepEqual(
    [...holder.querySelectorAll(".chip-id")].map((c) => c.textContent),
    ["model-b", "model-a"],
  );
  assert.equal(input.value, "");
  assert.equal(store.value, "model-b\nmodel-a");

  // A duplicate is ignored rather than rendered twice: the list is a set.
  input.value = "model-a";
  input.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  assert.equal(holder.querySelectorAll(".chip").length, 2);

  // Each chip removes only itself.
  holder.querySelector('[data-chip="model-b"]').dispatchEvent(new window.Event("click", { bubbles: true }));
  assert.deepEqual(
    [...holder.querySelectorAll(".chip-id")].map((c) => c.textContent),
    ["model-a"],
  );
  assert.equal(store.value, "model-a");
  // Removable chips need a name of their own: "x" alone says nothing.
  assert.match(holder.querySelector(".chip-x").getAttribute("aria-label"), /Remove model-a/);
});

test("a pasted comma list becomes one chip per id", async (t) => {
  const { window, dialog } = formHarness(t, { catalog: { models: [], catalog: "ok" } });
  const holder = dialog.querySelector("#f-approvedModels-chips");
  const input = dialog.querySelector("#f-approvedModels-input");
  const store = dialog.querySelector("#f-approvedModels");
  // The field this replaced was a one-per-line textarea, so a comma list is
  // what an operator's fingers produce.
  const paste = new window.Event("paste", { bubbles: true, cancelable: true });
  paste.clipboardData = { getData: () => "model-x, model-y\nmodel-z" };
  input.dispatchEvent(paste);
  assert.deepEqual(
    [...holder.querySelectorAll(".chip-id")].map((c) => c.textContent),
    ["model-x", "model-y", "model-z"],
  );
  assert.equal(store.value, "model-x\nmodel-y\nmodel-z");
});

test("a model id typed and then abandoned is kept, not silently dropped", async (t) => {
  const { window, dialog } = formHarness(t, { catalog: { models: [], catalog: "ok" } });
  const input = dialog.querySelector("#f-approvedModels-input");
  const store = dialog.querySelector("#f-approvedModels");
  input.value = "model-typed";
  // No Enter: the operator went straight for Save.
  input.dispatchEvent(new window.Event("blur"));
  assert.equal(store.value, "model-typed");
});

test("the chip field submits the same payload the textarea did", async (t) => {
  const { window, dialog, payloads } = formHarness(t, {
    catalog: { models: ["model-a"], catalog: "ok" },
    provider: { approvedModels: ["model-a", "model-zzz"] },
  });
  await new Promise((r) => setTimeout(r, 20));
  const form = dialog.querySelector("#actionForm");
  form.dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
  await new Promise((r) => setTimeout(r, 30));
  const save = payloads.find((p) => p.path === "pi-provider");
  assert.ok(save, `expected the save call, got ${JSON.stringify(payloads)}`);
  // The wire format is unchanged: a list of ids, in the order they were added,
  // so an id the catalog does not list yet (model-zzz) survives the save.
  assert.deepEqual(JSON.parse(JSON.stringify(save.body.approvedModels)), ["model-a", "model-zzz"]);
});
