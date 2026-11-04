// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/intro-actions.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

// Slice the helpers renderSettings needs out of the single page script.
const at = (marker) => script.indexOf(marker);
const helpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function badge("), at("function empty(")],
  [at("function empty("), at("function record(")],
  [at("function record("), at("function routeURL(")],
  [at("function button("), at("function facts(")],
  [at("function facts("), at("function notify(")],
  [at("function replaceContent("), at("function count(")],
  [at("const intros = {"), at("function renderSettings()")],
  [at("function renderSettings()"), at("async function changed(")],
  [at("function count("), at("function mountDialog(")],
]
  .map(([from, to]) => script.slice(from, to))
  .join("\n");

function settingsHarness(t, sub) {
  const dom = new JSDOM(
    `<div id="content"></div><span id="resultCount"></span><div id="toast" hidden></div>`,
    { url: "http://piso.local/" },
  );
  t.after(() => dom.window.close());
  const { window } = dom;
  const sandbox = {
    document: window.document,
    window,
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    $: (s) => window.document.querySelector(s),
    sub,
    query: "",
    loaded: new Set(["connections"]),
    data: {
      secrets: [],
      rules: [],
      patterns: [],
      exceptions: [],
      domains: [],
      mcp: [],
      connections: [],
      provider: {},
      routes: [],
      workers: [],
    },
    list: (key) => sandbox.data[key] || [],
    matches: () => true,
    refresh: async () => {},
    changed: async () => {},
    action: async () => {},
    api: async () => ({}),
    count: () => {},
    replaceContent: (html) => {
      const content = window.document.getElementById("content");
      if (content) content.innerHTML = html;
    },
    openExternal: () => {},
    routeURL: (name) => `http://${name}.piso.local`,
    ingressPort: 80,
    hideProject: false,
    esc: sandboxEsc,
  };
  function sandboxEsc(value) {
    return String(value ?? "").replace(/[&<>"']/g, "");
  }
  // Unknown globals resolve to undefined instead of ReferenceError, so the
  // harness depends on stubs for functions renderSettings actually calls,
  // not on guessing every identifier in the sliced source.
  const context = new Proxy(sandbox, {
    has: () => true,
    get: (target, key) => {
      if (key in target) return target[key];
      try {
        return globalThis[key];
      } catch {
        return undefined;
      }
    },
  });
  vm.createContext(context);
  vm.runInContext(`"use strict";\n${helpers}\nrenderSettings();`, context);
  window.renderSettings = () => vm.runInContext('renderSettings();', context);
  window.scrollTo = () => {};
  return window.document;
}
test("standalone action rows keep a gap before the panel they introduce", () => {
  // Telegram and AI Boost render their actions outside .section-intro, directly
  // above the panel the actions introduce. The intro's margin does not apply
  // there, so the row carries the same gap itself.
  assert.match(html, /\.section-intro \+ \.intro-actions\s*\{[^}]*margin-bottom:\s*20px/);
});

test("single-action sections keep the same right-aligned wrapper", (t) => {
  const document = settingsHarness(t, "secrets");
  const intro = document.querySelector(".section-intro");
  assert.ok(intro.querySelector('[data-action="secret-add"]'), "primary action still renders");
  const group = intro.querySelector(".intro-actions");
  assert.ok(group, "wrapper present so alignment is consistent across sections");
  assert.equal(group.querySelectorAll("button").length, 1);
});
