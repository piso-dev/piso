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

function settingsHarness(t, sub, mappings = [], githubUser = { connected: true, configured: true, login: "octocat" }) {
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
      githubMappings: mappings,
      githubUser,
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

test("GitHub mappings show shared routes, scope, conflicts and group-specific actions", (t) => {
  const document = settingsHarness(t, "github", [{
    secretId: "sec_shared", name: "Team <access>", envKey: "GH_TOKEN", placeholder: "piso_shared",
    workers: ["alpha"], allowedHosts: ["api.github.com"], conflicts: ["acme"], warnings: ["Repository listing incomplete"],
    apps: [
      {id: "app_a", name: "Personal app", appId: 10, status: "ok", installations: [{id:101, account:"Acme", repos:["Acme/repo"]}]},
      {id: "app_b", name: "Org app", appId: 20, status: "ok", installations: [{id:202, account:"acme"}]},
    ],
  }, {secretId:"sec_private", name:"Gateway only", gatewayOnly:true, apps:[], conflicts:[], warnings:[]}]);
  const groups = document.querySelectorAll(".github-mapping");
  assert.equal(groups.length, 2);
  assert.equal(groups[0].querySelector("h3").textContent, "Team <access>");
  assert.equal(groups[0].querySelectorAll("tbody tr").length, 2);
  assert.match(groups[0].textContent, /GH_TOKEN/);
  assert.match(groups[0].textContent, /piso_shared/);
  assert.match(groups[0].textContent, /alpha/);
  assert.match(groups[0].textContent, /Acme\/repo/);
  assert.match(groups[0].textContent, /Ambiguous — blocked/);
  assert.match(groups[0].textContent, /Repository listing incomplete/);
  assert.equal(groups[0].querySelector('[data-action="github-import"]').dataset.id, "sec_shared");
  assert.equal(groups[0].querySelector('[data-action="secret-edit"]').dataset.id, "sec_shared");
  assert.equal(groups[0].querySelector('[data-action="github-reconnect"]').dataset.id, "app_a");
  assert.match(groups[1].textContent, /No workers/);
  assert.match(groups[1].textContent, /No apps connected/);
});

for (const [label, user, action] of [
  ["unconfigured", {}, "github-user-config"],
  ["signed out or revoked", { configured: true, connected: false }, "github-user-authorize"],
]) {
  test(`GitHub ${label} gate hides cached mappings and management actions`, (t) => {
    const document = settingsHarness(t, "github", [{ name: "Hidden group", secretId: "secret", apps: [] }], user);
    assert.ok(document.querySelector(`[data-action="${action}"]`));
    assert.equal(document.querySelector(".github-mapping"), null);
    assert.equal(document.querySelector('[data-action="github-connect"]'), null);
    assert.equal(document.querySelector('[data-action="github-import"]'), null);
    assert.doesNotMatch(document.body.textContent, /Hidden group/);
  });
}

test("OAuth setup button is centered inside its gate below the description", (t) => {
  const document = settingsHarness(t, "github", [], {});
  const gate = document.querySelector(".empty.github-gate");
  assert.ok(gate);
  assert.match(gate.querySelector("p").textContent, /Add a GitHub OAuth App/);
  assert.ok(gate.querySelector("p + .intro-actions [data-action=github-user-config]"));
  assert.match(html, /\.github-gate\s*\{[^}]*text-align:\s*center/);
  assert.match(html, /\.github-gate \.intro-actions\s*\{[^}]*justify-content:\s*center/);
});

test("single-action sections keep the same right-aligned wrapper", (t) => {
  const document = settingsHarness(t, "secrets");
  const intro = document.querySelector(".section-intro");
  assert.ok(intro.querySelector('[data-action="secret-add"]'), "primary action still renders");
  const group = intro.querySelector(".intro-actions");
  assert.ok(group, "wrapper present so alignment is consistent across sections");
  assert.equal(group.querySelectorAll("button").length, 1);
});
