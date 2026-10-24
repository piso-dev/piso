// A routing group imported from another enrolled gateway must render as
// read-only: no create/install/refresh/remove buttons, and a note pointing at
// the gateway that owns it. Local groups keep their management actions.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { JSDOM } = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a, b) => script.slice(script.indexOf(a), script.indexOf(b));

function harness() {
  const dom = new JSDOM('', { url: 'http://piso.local' });
  const context = {
    document: dom.window.document,
    window: dom.window,
    esc: v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
  };
  vm.createContext(context);
  vm.runInContext(slice('function badge(', 'function empty(') + slice('function button(', 'function notify(') +
    slice('function githubAppRows(', 'function githubMapping(') + slice('function githubMapping(', 'function renderGithub('), context);
  return context;
}

const mirrored = {
  secretId: 'tc_rid', name: 'GitHub', placeholder: 'piso_github_peer', envKey: 'GH_TOKEN',
  workers: [], allowedHosts: ['api.github.com'], conflicts: [], warnings: [],
  apps: [{
    id: 'gha_tc_42', appId: 42, name: 'piso-peer', slug: 'piso-peer', status: 'ok', source: 'technocore',
    installations: [{ id: 7, account: 'octocat', repos: ['octocat/only-repo'] }],
  }],
};

const local = {
  ...mirrored,
  secretId: 'sec_local',
  apps: [{
    id: 'gha_local', appId: 43, name: 'piso-local', slug: 'piso-local', status: 'ok', source: 'local',
    installations: [{ id: 9, account: 'acme', repos: ['acme/repo'] }],
  }],
};

test('mirrored app rows expose no management actions', () => {
  const context = harness();
  const html = vm.runInContext(`githubAppRows(${JSON.stringify(mirrored)})`, context);
  assert.match(html, /octocat\/only-repo/, 'repository details still render');
  assert.match(html, /from another gateway/);
  assert.match(html, /Managed on another gateway/);
  for (const action of ['github-connect', 'github-install', 'github-reconnect', 'github-refresh', 'github-remove']) {
    assert.equal(html.includes(action), false, `${action} must not be offered`);
  }
});

test('local app rows keep every management action', () => {
  const context = harness();
  const html = vm.runInContext(`githubAppRows(${JSON.stringify(local)})`, context);
  for (const action of ['github-install', 'github-reconnect', 'github-refresh', 'github-remove']) {
    assert.equal(html.includes(action), true, `${action} must be offered`);
  }
  assert.equal(html.includes('from another gateway'), false);
});

test('mirrored group explains where the connection lives', () => {
  const context = harness();
  const html = vm.runInContext(`githubMapping(${JSON.stringify(mirrored)})`, context);
  assert.match(html, /configured on another enrolled gateway/);
  assert.match(html, /worker scope is set here/);
  // Worker scope is a local property of the group and stays editable.
  assert.match(html, /secret-edit/);
});

test('local group shows no peer notice', () => {
  const context = harness();
  const html = vm.runInContext(`githubMapping(${JSON.stringify(local)})`, context);
  assert.equal(html.includes('another enrolled gateway'), false);
});
