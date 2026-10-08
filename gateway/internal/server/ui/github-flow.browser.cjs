// Run with NODE_PATH pointing to playwright's node_modules directory:
// NODE_PATH=.tools/ui-check/node_modules node --test gateway/internal/server/ui/github-flow.browser.cjs
// Uses the real dashboard CSS/renderers with stubbed data; no GitHub mutations.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { chromium } = require('playwright');
const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const css = html.match(/<style>([\s\S]*?)<\/style>/)[1];
const script = html.match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a, b) => script.slice(script.indexOf(a), script.indexOf(b));
const helpers = [
  slice('const esc =', 'const endpoints'),
  slice('function badge(', 'function empty('),
  slice('function empty(', 'function record('),
  slice('function button(', 'function facts('),
  slice('function facts(', 'function notify('),
  slice('const intros = {', 'async function changed('),
].join('\n');
const group = {
  secretId: 'shared', name: 'Team access', envKey: 'GH_TOKEN',
  placeholder: 'piso_example', workers: ['alpha'], allowedHosts: ['api.github.com'],
  conflicts: ['acme'], apps: [
    { id: 'one', name: 'First app', appId: 1, status: 'ok', installations: [{ id: 11, account: 'Acme', repos: ['Acme/one', 'Acme/a-very-long-repository-name-that-must-not-widen-the-column'] }] },
    { id: 'two', name: 'Second app', appId: 2, status: 'ok', installations: [{ id: 22, account: 'acme' }] },
  ],
};

test('GitHub onboarding and panel spacing render at desktop and mobile sizes', async t => {
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.setContent(`<style>${css}</style><main><div id="content"></div></main>`);
  await page.addScriptTag({ content: `
    const $ = s => document.querySelector(s);
    let sub = 'github', data = {}, query = '';
    const list = key => data[key] || [];
    const matches = () => true;
    const count = () => {};
    const replaceContent = html => $('#content').innerHTML = html;
    ${helpers}
    window.renderFixture = fixture => { data = fixture; renderSettings(); };
  ` });
  for (const width of [1280, 390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    for (const [user, apps, current] of [
      [{}, [], '1. Configure sign-in'],
      [{ configured: true }, [], '2. Sign in'],
      [{ configured: true, connected: true }, [], '3. Connect an app'],
      [{ configured: true, connected: true }, [{id:'draft', appId:1, status:'needs-install'}], '4. Install on an account'],
      [{ configured: true, connected: true, login: 'octocat' }, group.apps, null],
    ]) {
      await page.evaluate(({ user, group, apps }) => window.renderFixture({
        githubUser: user, githubMappings: [{ ...group, apps }],
      }), { user, group, apps });
      const active = page.locator('[aria-current="step"] > span');
      assert.equal(await active.count(), current ? 1 : 0);
      if (current) assert.equal(await active.textContent(), current);
      if (!user.connected) assert.equal(await page.locator('.github-mapping').count(), 0);
      assert.equal(await page.locator('.github-mapping [data-action="github-import"], .github-mapping [data-action="github-create-group"]').count(), 0);
      assert.equal(await page.locator('.github-route-summary').count(), 0);
      assert.equal(await page.locator('.github-setup li.complete:not([aria-current])').count(), current ? Number(current[0]) - 1 : 4);
      if (apps === group.apps) {
        assert.equal(await page.locator('.github-conflict').count(), 2);
        const row = page.locator('.github-app-row').first();
        const widths = () => row.locator('td').evaluateAll(cells => cells.map(cell => cell.getBoundingClientRect().width));
        const before = await widths();
        await row.locator('td').first().click();
        assert.ok(await row.locator('.github-repositories').isVisible());
        assert.ok(await row.locator('[data-action="github-reconnect"]').isVisible());
        assert.deepEqual(await widths(), before);
        for (const selector of ['.actions > button', '.github-repositories > div']) {
          const boxes = await row.locator(selector).evaluateAll(elements => elements.map(el => {
            const r = el.getBoundingClientRect(); return {top:r.top, bottom:r.bottom, right:r.right};
          }));
          for (let i = 1; i < boxes.length; i++) {
            assert.ok(boxes[i].top > boxes[i-1].bottom);
            if (selector.includes('button')) assert.ok(Math.abs(boxes[i].right - boxes[0].right) < 1);
          }
        }
        await row.locator('[data-action="github-refresh"]').click();
        assert.equal(await row.locator('.github-row-toggle').getAttribute('aria-expanded'), 'true');
        await row.locator('.github-row-toggle').focus();
        await page.keyboard.press('Enter');
        assert.equal(await row.locator('.github-row-toggle').getAttribute('aria-expanded'), 'false');
        assert.deepEqual(await widths(), before);
        await page.keyboard.press('Space');
        assert.equal(await row.locator('.github-row-toggle').getAttribute('aria-expanded'), 'true');
      }
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `page overflow at ${width}px`);
      // Every displayed action stays inset from its closest painted panel or
      // table cell. Scroll each action into view, including mobile table actions.
      for (const button of await page.locator('#content button:visible').all()) {
        await button.scrollIntoViewIfNeeded();
        const clearance = await button.evaluate(el => {
          const panel = el.closest('td, .github-gate, .github-session, .github-mapping') || document.querySelector('main');
          const a = el.getBoundingClientRect(), b = panel.getBoundingClientRect();
          return { text: el.textContent, gaps: [a.left-b.left, b.right-a.right, a.top-b.top, b.bottom-a.bottom] };
        });
        assert.ok(clearance.gaps.every(gap => gap >= 6), `${width}px ${JSON.stringify(clearance)}`);
      }
    }
  }
  assert.deepEqual(errors, []);
});
