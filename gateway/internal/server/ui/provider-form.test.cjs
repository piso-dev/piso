// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/provider-form.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

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
  [at("function providerCard("), at("function providerForm(")],
  [at("function providerForm("), at("function addPolicy(")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

function formHarness(t, { catalog, fail = false, baseUrl = "https://api.example.test/v1", provider = {} } = {}) {
  const dom = new JSDOM(`<dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog><div id="toast" hidden></div>`, {
    url: "http://piso.local/",
  });
  t.after(() => dom.window.close());
  const { window } = dom;
  const calls = [];
  const sandbox = {
    document: window.document,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    KeyboardEvent: window.KeyboardEvent,
    Event: window.Event,
    api: async (path) => {
      calls.push(path);
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
  vm.runInContext(`"use strict";\n${helpers}\nproviderForm("p-1");`, sandbox);
  return { window, calls, dialog: window.document.querySelector("dialog") };
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
  assert.deepEqual(calls, ["pi-provider/models?id=p-1"]);
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
  assert.match(dialog.querySelector("#modelHint").textContent, /Save the provider first/);
  assert.equal(dialog.querySelector("#modelList").hidden, true);
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
