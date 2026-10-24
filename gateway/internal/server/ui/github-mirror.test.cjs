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

const localGroup = {
  ...mirrored,
  secretId: 'sec_local2',
  apps: [{ id: 'gha_local2', appId: 44, name: 'piso-local', status: 'ok', source: 'local', installations: [{ id: 10, account: 'acme' }] }],
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

// A gateway whose only connection arrived from a peer must not be walked
// through "configure sign-in → connect an app → install": none of that work
// belongs to it. The read-only rows and their scope editor are the whole page.
test('peer-only gateway shows no first-connection walkthrough', () => {
  const context = harness();
  vm.runInContext(slice('function githubSetup(', 'function setGithubRowExpanded('), context);
  const signedOut = { configured: false, connected: false };
  assert.equal(vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, ${JSON.stringify([mirrored])})`, context), '');
  // A gateway with its own app still gets the walkthrough.
  assert.notEqual(vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, ${JSON.stringify([local])})`, context), '');
  // No connections at all keeps the empty walkthrough.
  assert.notEqual(vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, [])`, context), '');
});

// The peer opens on "shared", not on "configure sign-in": the connection works
// for its workers and no OAuth App is needed to use it.
test('peer gateway without an OAuth app says the connection is shared', () => {
  const document = new JSDOM('<div id="content"></div><span id="resultCount"></span>', { url: 'http://piso.local' }).window.document;
  const context = {
    document,
    $: (s) => document.querySelector(s),
    data: { githubUser: { configured: false, connected: false }, githubMappings: [mirrored] },
    list: (key) => [mirrored],
    matches: () => true,
    count: () => {},
    replaceContent: (html) => { document.querySelector('#content').innerHTML = html; },
    esc: v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
    empty: (t, d) => `<section class="empty"><h2>${t}</h2><p>${d}</p></section>`,
    bindGithubRows: () => {},
    button: (label, action, id) => `<button data-action="${action}" data-id="${id ?? ''}">${label}</button>`,
    badge: (t) => `<span>${t}</span>`,
    facts: () => '',
    notify: () => {},
  };
  vm.createContext(context);
  vm.runInContext(slice('function githubSetup(', 'function renderTelegram('), context);
  vm.runInContext('renderGithub("")', context);
  const html = document.querySelector('#content').innerHTML;
  assert.match(html, /Shared from another gateway/);
  assert.doesNotMatch(html, /Configure GitHub sign-in<\/h2>/);
  assert.match(html, /its workers can use them now/);
  // A gateway with nothing at all still gets the first-connection gate.
  const fresh = { ...context, data: { githubUser: { configured: false, connected: false }, githubMappings: [] }, list: () => [] };
  vm.createContext(fresh);
  vm.runInContext(slice('function githubSetup(', 'function renderTelegram('), fresh);
  vm.runInContext('renderGithub("")', fresh);
  assert.match(fresh.document.querySelector('#content').innerHTML, /Configure GitHub sign-in<\/h2>/);
});

// The sign-in itself is shared too, and signing out is global. The page says
// both instead of implying the session is local to this gateway.
test('shared sign-in says where it came from and that sign-out is global', () => {
  const document = new JSDOM('<div id="content"></div><span id="resultCount"></span>', { url: 'http://piso.local' }).window.document;
  const context = {
    document,
    $: (s) => document.querySelector(s),
    data: { githubUser: { configured: true, connected: true, login: 'octocat', source: 'technocore' }, githubMappings: [mirrored] },
    list: (key) => (key === 'githubMappings' ? [mirrored] : []),
    matches: () => true,
    count: () => {},
    replaceContent: (html) => { document.querySelector('#content').innerHTML = html; },
    esc: v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
    empty: (t, d) => `<section class="empty"><h2>${t}</h2><p>${d}</p></section>`,
    bindGithubRows: () => {},
    button: (label, action, id) => `<button data-action="${action}" data-id="${id ?? ''}">${label}</button>`,
    badge: (t) => `<span>${t}</span>`,
    facts: () => '',
    notify: () => {},
    confirmAction: () => {},
  };
  vm.createContext(context);
  vm.runInContext(slice('function githubSetup(', 'function renderTelegram('), context);
  vm.runInContext('renderGithub("")', context);
  const html = document.querySelector('#content').innerHTML;
  assert.match(html, /signed in on another gateway/);
  assert.match(html, /Signing out here signs out every gateway/);
  // A locally started session carries no "another gateway" annotation.
  const local = { ...context, data: { githubUser: { configured: true, connected: true, login: 'octocat', source: 'local' }, githubMappings: [localGroup] }, list: () => [localGroup] };
  vm.createContext(local);
  vm.runInContext(slice('function githubSetup(', 'function renderTelegram('), local);
  vm.runInContext('renderGithub("")', local);
  const localHtml = local.document.querySelector('#content').innerHTML;
  assert.equal(localHtml.includes('signed in on another gateway'), false);
});
