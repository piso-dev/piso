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
  [at("function replaceContent("), at("function count(")],
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
    window,
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
  window.renderSettings = () => vm.runInContext('renderSettings();', context);
  window.scrollTo = () => {};
  return window.document;
}
test("github section groups Create GitHub App and Import existing app at the right edge", (t) => {
  const document = settingsHarness(t, "github");
  const intro = document.querySelector(".section-intro");
  assert.ok(intro, "section intro renders");
  const group = intro.querySelector(".intro-actions");
  assert.ok(group, "intro actions are wrapped in one right-aligned container");
  const buttons = [...group.querySelectorAll("button")];
  assert.deepEqual(
    buttons.map((b) => b.textContent.trim()),
    ["Create GitHub App", "Import existing app"],
    "both app actions live in the same group, primary first",
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
  assert.equal(groups[0].querySelectorAll('tr.github-conflict').length, 2);
  assert.equal(groups[0].querySelectorAll('.github-conflict .actions > [data-action="github-remove"]').length, 2);
  assert.match(groups[0].querySelector('.notice').textContent, /removes all of its routes/);
  assert.match(groups[0].querySelector('.github-route-details').textContent, /without an identifiable account require exactly one installation/);
  assert.equal(groups[0].querySelector('.github-route-details').open, false);
  assert.equal(groups[0].querySelector('.github-maintenance .actions').hidden, true);
  assert.match(groups[0].querySelector('.github-scope').textContent, /alpha/);
  for (const group of groups) {
    assert.equal(group.querySelector('[data-action="github-import"]'), null);
    assert.equal(group.querySelector('[data-action="github-create-group"]'), null);
    assert.equal(group.querySelector('.github-route-summary'), null);
    assert.doesNotMatch(group.textContent, /→ placeholder →/);
  }
  assert.equal(document.querySelectorAll('[data-action="github-import"]').length, 1);
  assert.equal(document.querySelectorAll('[data-action="github-connect"]').length, 1);
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

for (const [label, user, apps, step] of [
  ["unconfigured", {}, [], 1],
  ["signed out", { configured: true }, [{ appId: 1, status: "ok", installations: [{id: 1, account: "acme"}] }], 2],
  ["no apps", { configured: true, connected: true }, [], 3],
  ["draft", { configured: true, connected: true }, [{ id: "draft", status: "needs-register" }], 3],
  ["not installed", { configured: true, connected: true }, [{ id: "app", appId: 1, status: "needs-install" }], 4],
  ["installed", { configured: true, connected: true }, [{ id: "app", appId: 1, status: "ok", installations: [{id: 1, account: "acme"}] }], null],
]) {
  test(`GitHub first-connection progress: ${label}`, t => {
    const document = settingsHarness(t, "github", [{secretId: "s", apps}], user);
    const steps = [...document.querySelectorAll('.github-setup li')];
    assert.equal(steps.length, 4);
    const current = document.querySelector('[aria-current="step"]');
    assert.equal(current ? steps.indexOf(current) + 1 : null, step);
    assert.equal(document.querySelectorAll('.github-setup li.complete').length, step ? step - 1 : 4);
    for (const item of steps) {
      assert.equal(item.classList.contains('complete'), item.textContent.includes('Complete'));
      if (item.hasAttribute('aria-current')) assert.equal(item.classList.contains('complete'), false);
    }
    if (!step) assert.ok(steps.every(s => s.textContent.includes("Complete")));
    if (label === "draft") assert.ok(document.querySelector('tbody .primary[data-action="github-connect"]'));
    if (label === "not installed") assert.ok(document.querySelector('tbody .primary[data-action="github-install"]'));
    if (label === "installed") assert.equal(document.querySelector('tbody .primary'), null);
  });
}

test("completed GitHub steps use a restrained green treatment", () => {
  assert.match(html, /\.github-setup li\.complete\s*\{[^}]*border-color:\s*color-mix\(in srgb, var\(--green\) 45%, var\(--line\)\)/);
  assert.match(html, /\.github-setup li\.complete\s*\{[^}]*background:\s*color-mix\(in srgb, var\(--green\) 5%, transparent\)/);
  assert.match(html, /\.github-setup li\.complete small\s*\{[^}]*color:\s*var\(--green\)/);
});

test("GitHub panel and action containers keep edge clearance", () => {
  // Guard shared insets against accidental removal. These CSS assertions
  // are not a substitute for checking rendered geometry in a browser.
  for (const [selector, padding] of [
    ["github-gate", "32px 24px"], ["github-mapping", "20px"],
    ["record-body", "12px 16px 22px"], ["github-session", "16px"],
  ]) assert.match(html, new RegExp(`\\.${selector}\\s*\\{[^}]*padding:\\s*${padding}`));
  assert.match(html, /\.github-maintenance \.actions\s*\{[^}]*flex-direction:\s*column;\s*align-items:\s*flex-end/);
  assert.match(html, /\.github-routing-table\s*\{[^}]*table-layout:\s*fixed/);
  assert.doesNotMatch(html, /\.github-maintenance \.actions\s*\{[^}]*background/);
  assert.match(html, /\.github-setup li\s*\{[^}]*padding:\s*18px 16px/);
  assert.match(html, /\.github-setup li\.complete\s*\{[^}]*padding:\s*20px 18px/);
  assert.match(html, /\.github-row-actions\s*\{[^}]*gap:\s*12px/);
});

test("GitHub row expands repositories and actions together without action clicks collapsing it", t => {
  const document = settingsHarness(t, 'github', [{ secretId: 's', apps: [{
    id: 'app', name: 'Test app', appId: 1, status: 'ok', installations: [
      {id: 1, account: 'acme', repos: ['acme/one', 'acme/two']},
      {id: 2, account: 'other'},
    ],
  }] }]);
  let rows = [...document.querySelectorAll('.github-app-row')];
  let toggle = rows[0].querySelector('.github-row-toggle');
  const expanded = row => row.querySelector('.github-row-toggle').getAttribute('aria-expanded');
  assert.equal(expanded(rows[0]), 'false');
  rows[0].cells[1].click(); // Not the Manage app control.
  assert.equal(expanded(rows[0]), 'true');
  assert.equal(expanded(rows[1]), 'false');
  assert.ok([...rows[0].querySelectorAll('[data-row-detail]')].every(el => !el.hidden));
  assert.deepEqual([...rows[0].querySelectorAll('.github-repositories > div')].map(el => el.textContent), ['acme/one', 'acme/two']);
  assert.equal(rows[0].querySelectorAll('.actions button').length, 4);
  rows[0].querySelector('[data-action="github-refresh"]').click();
  assert.equal(expanded(rows[0]), 'true');
  toggle.focus();
  document.defaultView.renderSettings(); // Polling preserves disclosure state and focus.
  rows = [...document.querySelectorAll('.github-app-row')];
  toggle = rows[0].querySelector('.github-row-toggle');
  assert.equal(expanded(rows[0]), 'true');
  assert.equal(document.activeElement, toggle);
  for (const id of toggle.getAttribute('aria-controls').split(' ')) assert.ok(document.getElementById(id));
  assert.equal(document.querySelectorAll('.github-routing-table col').length, 5);
  toggle.click(); // Native button is also keyboard-accessible.
  assert.equal(expanded(rows[0]), 'false');
  assert.ok([...rows[0].querySelectorAll('[data-row-detail]')].every(el => el.hidden));
  rows[1].cells[0].click();
  assert.equal(expanded(rows[1]), 'true');
  assert.equal(rows[1].querySelectorAll('.actions button').length, 4);
  assert.match(rows[1].querySelector('.github-repositories').textContent, /No repositories listed/);
});

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
