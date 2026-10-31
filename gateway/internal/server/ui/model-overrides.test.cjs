// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/model-overrides.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

const at = (marker) => {
  const index = script.indexOf(marker);
  assert.notEqual(index, -1, `missing marker: ${marker}`);
  return index;
};
const helpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function badge("), at("function field(")],
  [at("function field("), at("function formDialog(")],
  [at("function button("), at("function facts(")],
  [at("function notify("), at("function mountDialog(")],
  [at("function mountDialog("), at("function closeDialog(")],
  [at("function closeDialog("), at("function field(")],
  [at("function formDialog("), at("function confirmAction(")],
  // The Tinfoil constants and hostOf, which the override dialog reads to decide
  // whether a provider's model field is optional.
  [at("const TINFOIL_BASE_URL ="), at("function overrideData(")],
  // overrideData + the combobox/field helpers + the section and dialog.
  [at("function overrideData("), at("function providerForm(")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

function harness(t, overrides, { workers, providers } = {}) {
  const dom = new JSDOM(
    `<dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog><div id="toast" hidden></div>`,
    { url: "http://piso.local/" },
  );
  t.after(() => dom.window.close());
  const { window } = dom;
  const calls = [];
  const toasts = [];
  const sandbox = {
    document: window.document,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    KeyboardEvent: window.KeyboardEvent,
    FormData: window.FormData,
    toastTimer: null,
    Event: window.Event,
    api: async (path, method, body) => {
      calls.push({ path, method, body });
      return { ok: true };
    },
    dialogBusy: false,
    dialogReturn: null,
    data: {
      workers: workers || [
        { name: "piso-worker-alpha", slug: "alpha" },
        { name: "piso-worker-beta", slug: "beta" },
        { name: "piso-worker-gamma", slug: "gamma" },
      ],
      modelOverrides: {
        overrides: overrides || [],
        account: { providerId: "piso", providerName: "AIML", model: "model-a" },
        providers: providers || [
          { id: "piso", name: "AIML", defaultModel: "model-a" },
          { id: "piso-two", name: "Second", defaultModel: "model-b", workers: ["alpha"] },
        ],
      },
    },
    list: (key) => sandbox.data[key] || [],
    changed: async (keys) => calls.push({ path: `changed:${keys.join(",")}` }),
  };
  window.HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
  };
  sandbox.$ = (s) => window.document.querySelector(s);
  vm.createContext(sandbox);
  return { window, calls, toasts, run: (code) => vm.runInContext(`"use strict";\n${helpers}\n${code}`, sandbox) };
}

test("the override list says who is pinned and who still inherits", (t) => {
  const h = harness(t, [
    { slug: "alpha", providerId: "piso-two", providerName: "Second", model: "model-b", source: "override" },
    {
      slug: "beta",
      providerId: "piso-gone",
      providerName: "",
      source: "account",
      warning: "the pinned provider no longer exists; this worker uses the account default",
    },
  ]);
  const markup = h.run("overrideSection()");

  // One row per pinned worker, in the order the gateway returned them.
  assert.equal((markup.match(/class="override-row"/g) || []).length, 2);
  assert.match(markup, /alpha/);
  assert.match(markup, /Second/);
  assert.match(markup, /model-b/);
  // A deleted provider is surfaced, not silently dropped: the operator's
  // intent is still visible while the worker quietly uses the default.
  assert.match(markup, /pinned provider no longer exists/);
  assert.match(markup, /piso-gone/);
  // The account line counts the workers that do NOT have a row.
  assert.match(markup, /1 worker follows it/);
  // Remove is per row and carries the slug, not a provider id.
  assert.match(markup, /data-action="override-remove" data-id="alpha"/);
});

test("an empty override table reads as the norm, not as a broken list", (t) => {
  const h = harness(t, []);
  const markup = h.run("overrideSection()");
  assert.doesNotMatch(markup, /class="override-row"/);
  assert.match(markup, /Every worker follows the account default/);
  assert.match(markup, /3 workers follow it/);
});

test("adding an override sends the slug and provider, then refreshes the list", (t) => {
  const h = harness(t, [{ slug: "beta", providerId: "piso", providerName: "AIML" }]);
  h.run("overrideForm()");
  const doc = h.window.document;

  // Workers that already have an override are not offered again: the slug is
  // the key, so a second row could only ever replace the first.
  const workerOptions = [...doc.querySelectorAll("#f-slug option")].map((o) => o.value);
  assert.deepEqual(workerOptions, ["alpha", "gamma"]);

  // The scoped provider is still selectable here, but the label says so.
  const providerOptions = [...doc.querySelectorAll("#f-providerId option")].map((o) => o.textContent);
  assert.match(providerOptions[1], /\(secret granted to one worker\)/);

  // Submitting drives a PUT, not a config rewrite.
  const form = doc.querySelector("#dialogContent form");
  form.querySelector("#f-slug").value = "alpha";
  form.querySelector("#f-providerId").value = "piso-two";
  form.dispatchEvent(new h.window.Event("submit", { bubbles: true, cancelable: true }));

  return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
    const put = h.calls.find((c) => c.method === "PUT");
    assert.deepEqual(JSON.parse(JSON.stringify(put)), {
      path: "model-overrides",
      method: "PUT",
      // An empty model is the documented "follow the provider's default", so it
      // is sent explicitly rather than omitted.
      body: { slug: "alpha", providerId: "piso-two", model: "" },
    });
    assert.ok(h.calls.some((c) => c.path === "changed:modelOverrides"));
  });
});

test("every row offers an edit action that opens the same dialog", (t) => {
  const h = harness(t, [{ slug: "alpha", providerId: "piso", providerName: "AIML" }]);
  const markup = h.run("overrideSection()");
  assert.match(markup, /data-action="override-edit" data-id="alpha"/);
});

test("editing shows the pinned model, not the substitute the worker is running", (t) => {
  // A drifted row: model is what the worker actually runs, pinnedModel is what
  // the operator asked for. Prefilling the substitute would rewrite the intent
  // on save without ever showing that it changed.
  const h = harness(t, [
    {
      slug: "alpha",
      providerId: "piso-two",
      providerName: "Second",
      model: "model-b-substitute",
      pinnedModel: "model-b",
      drifted: true,
      driftState: "missing",
    },
  ]);
  h.run("overrideForm(\"alpha\")");
  const doc = h.window.document;

  // The worker is the key, so the dialog states it instead of offering a
  // dropdown whose only correct answer is the value it already has.
  assert.equal(doc.querySelector("#f-slug"), null);
  assert.equal(doc.querySelector('input[name="slug"]').value, "alpha");
  assert.match(doc.querySelector("#dialogContent").textContent, /alpha/);

  assert.equal(doc.querySelector("#f-providerId").value, "piso-two");
  assert.equal(doc.querySelector("#f-model").value, "model-b");

  const form = doc.querySelector("#dialogContent form");
  form.dispatchEvent(new h.window.Event("submit", { bubbles: true, cancelable: true }));
  return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
    const put = h.calls.find((c) => c.method === "PUT");
    assert.deepEqual(JSON.parse(JSON.stringify(put)), {
      path: "model-overrides",
      method: "PUT",
      body: { slug: "alpha", providerId: "piso-two", model: "model-b" },
    });
  });
});

test("a removed provider is named, not silently replaced by the first one", (t) => {
  const h = harness(t, [
    { slug: "alpha", providerId: "piso-gone", providerName: "", source: "account" },
  ]);
  h.run("overrideForm(\"alpha\")");
  const doc = h.window.document;

  // A <select> with no matching option reports the first provider, so saving
  // would repoint the worker at a provider nobody chose.
  assert.equal(doc.querySelector("#f-providerId").value, "");
  assert.match(doc.querySelector("#f-providerId option").textContent, /Provider removed/);

  const form = doc.querySelector("#dialogContent form");
  form.dispatchEvent(new h.window.Event("submit", { bubbles: true, cancelable: true }));
  return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
    assert.equal(h.calls.find((c) => c.method === "PUT"), undefined);
    assert.match(doc.querySelector("#formError").textContent, /no longer in the stack/);
  });
});

test("a provider with nothing to pin to is disabled, with the reason in its label", (t) => {
  const h = harness(t, [], {
    providers: [
      { id: "piso", name: "AIML", defaultModel: "model-a", pinnable: true },
      { id: "piso-empty", name: "Empty", defaultModel: "", pinnable: false },
    ],
  });
  h.run("overrideForm()");
  const options = [...h.window.document.querySelectorAll("#f-providerId option")];
  assert.equal(options[1].disabled, true);
  assert.match(options[1].textContent, /no usable model yet/);
  assert.equal(options[0].disabled, false);
});

test("a tinfoil provider makes the model optional and says why", (t) => {
  const h = harness(t, [], {
    providers: [
      {
        id: "piso-tinfoil",
        name: "Tinfoil",
        kind: "tinfoil",
        baseUrl: "http://127.0.0.1:3301/v1",
        pinnable: true,
      },
    ],
  });
  h.run("overrideForm()");
  const doc = h.window.document;
  // The enclave proxy inside the worker serves these requests, so the gateway
  // never judges the model. Saying that is the difference between "optional"
  // and "unchecked".
  assert.match(doc.querySelector("#f-model-hint").textContent, /enclave proxy/);
  assert.equal(doc.querySelector("#f-model-hint").hidden, false);

  const form = doc.querySelector("#dialogContent form");
  form.querySelector("#f-slug").value = "alpha";
  form.dispatchEvent(new h.window.Event("submit", { bubbles: true, cancelable: true }));
  return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
    const put = h.calls.find((c) => c.method === "PUT");
    assert.deepEqual(JSON.parse(JSON.stringify(put.body)), {
      slug: "alpha",
      providerId: "piso-tinfoil",
      model: "",
    });
  });
});

test("changing the provider clears a model it does not approve", (t) => {
  const h = harness(t, [], {
    providers: [
      { id: "piso", name: "AIML", defaultModel: "model-a", allowedModels: ["model-a"] },
      { id: "piso-two", name: "Second", defaultModel: "model-b", allowedModels: ["model-b"] },
    ],
  });
  h.run("overrideForm()");
  const doc = h.window.document;
  const model = doc.querySelector("#f-model");
  model.value = "model-a";

  const select = doc.querySelector("#f-providerId");
  select.value = "piso-two";
  select.dispatchEvent(new h.window.Event("change", { bubbles: true }));

  // Keeping it would be a save that can only 400; empty already means "follow
  // the default", so the dialog says what it dropped.
  assert.equal(model.value, "");
  assert.match(doc.querySelector("#f-model-hint").textContent, /not approved by this provider, so it was cleared/);
});

test("the model field suggests the provider's approved models, not its catalog", (t) => {
  const h = harness(t, [], {
    providers: [
      { id: "piso", name: "AIML", defaultModel: "model-a", allowedModels: ["model-a", "model-z"] },
    ],
  });
  h.run("overrideForm()");
  const doc = h.window.document;
  const model = doc.querySelector("#f-model");
  model.value = "model";
  model.dispatchEvent(new h.window.Event("input", { bubbles: true }));
  const offered = [...doc.querySelectorAll("#f-model-list li")].map((li) => li.dataset.model);
  assert.deepEqual(offered, ["model-a", "model-z"]);
});

test("adding skips a provider with no usable model instead of opening on it", (t) => {
  const h = harness(t, [], {
    providers: [
      { id: "piso-empty", name: "Empty", defaultModel: "", pinnable: false },
      { id: "piso", name: "AIML", defaultModel: "model-a", pinnable: true },
    ],
  });
  h.run("overrideForm()");
  const select = h.window.document.querySelector("#f-providerId");
  assert.equal(select.value, "piso");
});

test("adding says so when no provider can be pinned to", (t) => {
  const h = harness(t, [], {
    providers: [{ id: "piso-empty", name: "Empty", defaultModel: "", pinnable: false }],
  });
  h.run("overrideForm()");
  // No dialog whose only possible save is a 400: the reason is the message.
  assert.equal(h.window.document.querySelector("#dialogContent").innerHTML, "");
  assert.match(h.window.document.querySelector("#toast").textContent, /needs a default model or at least one approved model/);
});

test("the dialog refuses to open when there is nothing to override", (t) => {
  const h = harness(t, [], { workers: [], providers: [{ id: "piso", name: "AIML" }] });
  h.run("overrideForm()");
  assert.equal(h.window.document.querySelector("#dialogContent form"), null);
  assert.match(h.window.document.querySelector("#toast").textContent, /No workers have registered yet/);
});

test("no providers means no override — the dialog says why instead of opening empty", (t) => {
  const h = harness(t, [], { providers: [] });
  h.run("overrideForm()");
  assert.equal(h.window.document.querySelector("#dialogContent form"), null);
  assert.match(h.window.document.querySelector("#toast").textContent, /Add a provider/);
});
