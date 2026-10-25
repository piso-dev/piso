// Which gateway a connection came from is provenance, not presentation. The
// envelope is authoritative and any gateway may publish, so a group that
// arrived from another enrolled gateway renders and behaves exactly like a
// local one — same rows, same actions, same page.
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

test('an app from another gateway offers every management action', () => {
  const context = harness();
  const html = vm.runInContext(`githubAppRows(${JSON.stringify(mirrored)})`, context);
  assert.match(html, /octocat\/only-repo/, 'repository details still render');
  for (const action of ['github-install', 'github-reconnect', 'github-refresh', 'github-remove']) {
    assert.equal(html.includes(action), true, `${action} must be offered`);
  }
  // No provenance caveat on the row: it is editable here like any other.
  assert.equal(html.includes('from another gateway'), false);
  assert.equal(html.includes('Managed on another gateway'), false);
});

test('every gateway renders the same row actions for the same app', () => {
  const context = harness();
  const fromPeer = vm.runInContext(`githubAppRows(${JSON.stringify(mirrored)})`, context);
  const fromLocal = vm.runInContext(`githubAppRows(${JSON.stringify(local)})`, context);
  const actions = (html) => [...html.matchAll(/data-action="([^"]+)"/g)].map((m) => m[1]).join(',');
  assert.equal(actions(fromPeer), actions(fromLocal));
  assert.match(fromPeer, /Manage app/);
  assert.match(fromLocal, /Manage app/);
});

test('local app rows keep every management action', () => {
  const context = harness();
  const html = vm.runInContext(`githubAppRows(${JSON.stringify(local)})`, context);
  for (const action of ['github-install', 'github-reconnect', 'github-refresh', 'github-remove']) {
    assert.equal(html.includes(action), true, `${action} must be offered`);
  }
  assert.equal(html.includes('from another gateway'), false);
});

test('a received group shows no peer notice and keeps scope editing', () => {
  const context = harness();
  const html = vm.runInContext(`githubMapping(${JSON.stringify(mirrored)})`, context);
  assert.equal(html.includes('another enrolled gateway'), false);
  // Worker scope is per-gateway and stays editable everywhere.
  assert.match(html, /secret-edit/);
});

// renderPage is the whole Access > GitHub page for one gateway's data. Every
// parity test below renders through it, so "identical" means the same markup,
// not merely the same vocabulary.
function renderPage(github, groups) {
  const document = new JSDOM('<div id="content"></div><span id="resultCount"></span>', { url: 'http://piso.local' }).window.document;
  const context = {
    document,
    $: (s) => document.querySelector(s),
    data: { githubUser: github, githubMappings: groups },
    list: () => groups,
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
  return document.querySelector('#content').innerHTML;
}

// Signing out is a fleet-wide state, so out of a session every gateway shows
// the same gate and the same read-only rows — whether the connection was made
// here or elsewhere.
test('signed out looks the same on every gateway', () => {
  const render = (groups) => renderPage({ configured: false, connected: false }, groups);
  const peerHtml = render([mirrored]);
  assert.match(peerHtml, /Configure GitHub sign-in<\/h2>/, 'the gate is the same as anywhere else');
  assert.equal(peerHtml.includes('Shared from another gateway'), false);
  // Routes stay visible, actions do not: they act as the signed-in account.
  assert.match(peerHtml, /octocat\/only-repo/);
  for (const action of ['github-install', 'github-reconnect', 'github-refresh', 'github-remove']) {
    assert.equal(peerHtml.includes(action), false, `${action} is withheld while signed out`);
  }
  assert.equal(peerHtml.includes('signed in on another gateway'), false);
  // Nothing connected at all renders the same gate.
  assert.match(render([]), /Configure GitHub sign-in<\/h2>/);
  // Same data, different origin: byte-identical markup, not just no caveat.
  assert.equal(peerHtml, renderPage({ configured: false, connected: false, source: 'local' }, [mirrored]));
});

// Every gateway is walked through the same first-connection steps, because any
// of them can do the work.
test('every gateway gets the same first-connection walkthrough', () => {
  const context = harness();
  vm.runInContext(slice('function githubSetup(', 'function setGithubRowExpanded('), context);
  const signedOut = { configured: false, connected: false };
  const fromPeer = vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, ${JSON.stringify([mirrored])})`, context);
  const fromLocal = vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, ${JSON.stringify([local])})`, context);
  assert.notEqual(fromPeer, '');
  assert.equal(fromPeer, fromLocal);
  assert.match(fromPeer, /Configure sign-in/);
  assert.notEqual(vm.runInContext(`githubSetup(${JSON.stringify(signedOut)}, [])`, context), '');
});

// Which gateway minted the session is provenance, not presentation: the page
// must render identically whether github-user arrived locally or from a peer.
// A difference here is what produced two different Access > GitHub layouts.
test('the GitHub page is byte-identical whatever the session source', () => {
  const base = { configured: true, connected: true, login: 'octocat', orgAccess: true };
  const peer = renderPage({ ...base, source: 'technocore' }, [mirrored]);
  const here = renderPage({ ...base, source: 'local' }, [mirrored]);
  assert.equal(peer, here);
  // Signed out is the same story, with and without a connection to show.
  for (const groups of [[mirrored], [localGroup], []]) {
    const out = { configured: true, connected: false, orgAccess: null };
    assert.equal(
      renderPage({ ...out, source: 'technocore' }, groups),
      renderPage({ ...out, source: 'local' }, groups),
    );
  }
  // The missing-scope prompt is a real state, and it is also source-blind.
  const noOrg = { ...base, orgAccess: false };
  assert.equal(
    renderPage({ ...noOrg, source: 'technocore' }, [mirrored]),
    renderPage({ ...noOrg, source: 'local' }, [mirrored]),
  );
  // No provenance vocabulary survives anywhere on the page.
  const signedOutPeer = renderPage({ configured: true, connected: false, orgAccess: null, source: 'technocore' }, [mirrored]);
  for (const html of [peer, signedOutPeer]) {
    assert.equal(/another (enrolled )?gateway/i.test(html), false, 'the page names no other gateway');
  }
});

// The render test above would pass for any pair of gateways that happen to
// agree; this one fails the moment a provenance branch is reintroduced, even
// in a code path the fixtures do not reach.
test('the page script never reads the session source', () => {
  assert.equal(/\bgithub\??\.source\b|\bgithubUser\??\.source\b/.test(script), false);
});
