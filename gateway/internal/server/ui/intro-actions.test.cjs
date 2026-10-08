// Requires jsdom. Run with NODE_PATH pointing to its node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/intro-actions.test.cjs
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const { JSDOM } = require("jsdom");
const vm = require("node:vm");
const html = fs.readFileSync(`${__dirname}/index.html`, "utf8");
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];

// Slice the helpers renderSettings needs out of the single page script.
const at = (marker) => script.indexOf(marker);
const helpers = [
  [at("const esc ="), at("const endpoints")],
  [at("function badge("), at("function empty(")],
  [at("function empty("), at("function record(")],
  [at("function record("), at("function routeURL(")],
  [at("function button("), at("function facts(")],
  [at("function facts("), at("function notify(")],
  [at("const intros = {"), at("function renderSettings()")],
  [at("function renderSettings()"), at("async function changed(")],
  [at("function count("), at("function mountDialog(")],,
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
    setTimeout: window.setTimeout,
    clearTimeout: window.clearTimeout,
    $: (s) => window.document.querySelector(s),
    sub,
    query: "",
    loaded: new Set(["github", "githubPending", "connections"]),
    data: {
      github: [
        {
          id: "gha_1",
          name: "piso-technocore",
          placeholder: "piso_github_app_piso-technocore",
          status: "ok",
          installations: [{ id: 9, account: "complex-gh", repos: ["complex-gh/piso"] }],
        },
      ],
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
  return window.document;
}
test("github section groups Connect GitHub and Connect existing app at the right edge", (t) => {
  const document = settingsHarness(t, "github");
  const intro = document.querySelector(".section-intro");
  assert.ok(intro, "section intro renders");
  const group = intro.querySelector(".intro-actions");
  assert.ok(group, "intro actions are wrapped in one right-aligned container");
  const buttons = [...group.querySelectorAll("button")];
  assert.deepEqual(
    buttons.map((b) => b.textContent.trim()),
    ["Connect GitHub", "Connect existing app"],
    "both connect actions live in the same group, primary first",
  );
  assert.equal(
    group.querySelector('[data-action="github-connect"]'),
    buttons[0],
  );
  assert.equal(
    group.querySelector('[data-action="github-import"]'),
    buttons[1],
  );
  // Container-level alignment: the group is the last flex child and the
  // stylesheet pins it to the right with a shared gap.
  const css = fs.readFileSync(`${__dirname}/index.html`, "utf8");
  assert.match(css, /\.intro-actions\s*{[^}]*justify-content:\s*flex-end/);
  assert.match(css, /\.intro-actions\s*{[^}]*gap:\s*8px/);
});

test("single-action sections keep the same right-aligned wrapper", (t) => {
  const document = settingsHarness(t, "secrets");
  const intro = document.querySelector(".section-intro");
  assert.ok(intro.querySelector('[data-action="secret-add"]'), "primary action still renders");
  const group = intro.querySelector(".intro-actions");
  assert.ok(group, "wrapper present so alignment is consistent across sections");
  assert.equal(group.querySelectorAll("button").length, 1);
});
