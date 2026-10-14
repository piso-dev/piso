const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const {JSDOM} = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a,b) => script.slice(script.indexOf(a),script.indexOf(b));
function harness(t) {
 const dom = new JSDOM('<div id="content"></div><span id="resultCount"></span><div id="toast"></div><dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog>',{url:'http://piso.local'});
 t.after(()=>dom.window.close());
 const calls=[];
 const sandbox={document:dom.window.document,window:dom.window,location:dom.window.location,URL,count:()=>{},sub:'aiBoost',query:'',loaded:new Set(['aiBoost']),
  data:{aiBoost:{configured:true}},
  $:s=>dom.window.document.querySelector(s), matches:()=>true,list:()=>[],hideProject:false,ingressPort:80,
  api:async(path,method,body)=>{calls.push({path,method,body});return {url:'https://piso.example/oauth/ai-boost/start?ticket=one'}},
  changed:async()=>{},refresh:async()=>{},action:async()=>{},errorDialog:e=>{throw e},safeURL:s=>s,
  dialogBusy:false, notify:message=>{sandbox.notification=message},closeDialog:()=>{dom.window.document.querySelector("#dialogContent").innerHTML=""},
  confirmAction:(...args)=>{sandbox.confirmation=args},formDialog:(...args)=>{sandbox.form=args},mountDialog:(...args)=>{sandbox.dialog=args;dom.window.document.querySelector("#dialog").className=args[2] || "";dom.window.document.querySelector("#dialogContent").innerHTML=args[1]},
 };
 const ctx=new Proxy(sandbox,{has:()=>true,get:(o,k)=>k in o?o[k]:globalThis[k]});vm.createContext(ctx);
 const helpers=[
  slice('const esc =','const endpoints'),slice('function badge(','function empty('),slice('function empty(','function record('),
  slice('function record(','function routeURL('),slice('function button(','function facts('),slice('function facts(','function notify('),
  slice('function replaceContent(','function count('),slice('function count(','function mountDialog('),
  slice('const intros = {','function renderSettings()'),slice('function renderSettings()','async function changed('),
  slice('const boostReview =','async function githubUserForm()'),slice('function field(','function formDialog(')
 ].join('\n');
 vm.runInContext(helpers,ctx);
 return {dom,sandbox,ctx,calls,run:s=>vm.runInContext(s,ctx)};
}
test('AI Boost tab renders one-click connect, reconnect and disconnect controls',t=>{
 const h=harness(t);h.run('renderSettings()');
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-authorize"]'));
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-disconnect"]'));
 assert.match(h.dom.window.document.body.textContent,/Connect AI Boost/);
 h.sandbox.data.aiBoost.configured=false;h.run('renderSettings()');
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-authorize"]'));
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-config"]'),null);
 assert.match(h.dom.window.document.body.textContent,/Connect AI Boost/);
 assert.doesNotMatch(h.dom.window.document.body.textContent,/Callback URL|API origin|Configure AI Boost/);
 h.sandbox.data.aiBoost.configured=true;h.sandbox.data.aiBoost.status='ok';h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Reconnect/);
});
test('sign-in opens synchronously and obtains a host POST ticket before navigating',async t=>{
 const h=harness(t);h.sandbox.data.aiBoost.configured=false;
 const tab={opener:{},location:{},close(){}};h.sandbox.window.open=()=>tab;
 h.run('boostStart()');assert.equal(tab.opener,null);
 await new Promise(resolve=>setImmediate(resolve));
 assert.equal(h.calls.length,1);
 assert.equal(h.calls[0].path,'ai-boost/authorize');assert.equal(h.calls[0].method,'POST');
 assert.equal(JSON.stringify(h.calls[0].body),'{}');
 assert.equal(h.sandbox.form,undefined);
 assert.match(tab.location.href,/\/oauth\/ai-boost\/start\?ticket=/);
});
test('disconnect clearly states local-only and requires confirmation',async t=>{
 const h=harness(t);h.run('boostDisconnect()');
 assert.match(h.sandbox.confirmation[1],/grant is not revoked/);assert.equal(h.calls.length,0);
 await h.sandbox.confirmation[3]();assert.equal(h.calls[0].path,'ai-boost/disconnect');assert.equal(h.calls[0].method,'POST');
});
test('AI Boost tab follows GitHub and renders fetch, pagination, setup and errors',async t=>{
 const h=harness(t);
 assert.match(script,/\["github", "GitHub"\],\s*\["aiBoost", "AI Boost"\]/);
 h.sandbox.sub='aiBoost';h.sandbox.data.aiBoost.status='ok';
 h.run('renderSettings()');assert.doesNotMatch(h.dom.window.document.body.textContent,/Review access is not configured|separate reviewer/);
 h.sandbox.api=async()=>({items:[{id:'one',name:'<img src=x onerror=alert(1)>',status:'submitted',revision:3}],nextCursor:'opaque'});
 await h.run('loadBoostDrafts()');
 assert.equal(h.dom.window.document.querySelector('img'),null);
 const fetch=h.dom.window.document.querySelector('[data-action="boost-review-refresh"]');
 assert.ok(fetch.querySelector('svg[aria-hidden="true"]'));
 assert.ok(fetch.closest('.boost-review-toolbar'));
 assert.equal(fetch.getAttribute('aria-busy'),'false');
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-review-page"]').dataset.id,'opaque');
 h.sandbox.api=async()=>{throw new Error('Queue unavailable')};
 await h.run('loadBoostDrafts()');
 assert.match(h.dom.window.document.querySelector('[role="alert"]').textContent,/Queue unavailable/);
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-preview"]'),null);
});
test('review layout keeps a single scroller and equal-width pinned actions',()=>{
 const css=fs.readFileSync(`${__dirname}/index.html`,'utf8').match(/<style>([\s\S]*)<\/style>/)[1];
 assert.match(css,/\.boost-review-toolbar\s*\{[^}]*padding-top: 20px/);
 assert.match(css,/\.boost-review-scroll\s*\{[^}]*min-height: 0;[^}]*overflow-y: auto/);
 assert.match(css,/\.boost-review-footer\s*\{[^}]*flex: 0 0 auto/);
 assert.match(css,/\.boost-review-decisions\s*\{[^}]*grid-template-columns: minmax\(0, 1fr\) minmax\(0, 1fr\)/);
 assert.match(css,/\.boost-review-decisions button\s*\{[^}]*min-height: 52px/);
 assert.match(css,/@media \(prefers-reduced-motion: no-preference\)\s*\{\s*\.boost-fetch/);
});
function submit(h, decision) {
 const form=h.dom.window.document.querySelector('#boostReviewForm');
 return form.onsubmit({preventDefault(){},submitter:form.querySelector('[value="'+decision+'"]')});
}
test('preview stacks details above Markdown, with two footer actions outside the scroll area',async t=>{
 const h=harness(t);
 h.sandbox.api=async()=>({id:'one',name:'Review',status:'submitted',revision:7,content:{body:'<script>evil()</script>'},destination:{name:'Library',tagId:'tag-uuid',visibility:'private'}});
 await h.run('boostPreview("one")');
 const doc=h.dom.window.document;
 assert.ok(doc.querySelector('dialog.boost-review-dialog'));
 const scroll=doc.querySelector('.boost-review-scroll');
 assert.match(scroll.textContent,/tag-uuid/);assert.match(scroll.textContent,/private/);
 assert.equal(scroll.querySelector('script'),null);
 assert.equal(scroll.querySelector('pre').textContent,'<script>evil()</script>');
 assert.ok(scroll.querySelector('.facts').compareDocumentPosition(scroll.querySelector('.boost-markdown')) & 4);
 assert.equal(scroll.querySelector('button'),null);
 assert.deepEqual([...doc.querySelectorAll('.boost-review-footer button')].map(b=>b.textContent),['Reject','Create new booster']);
 assert.equal(doc.querySelector('select[name="decision"]'),null);
});
test('version drafts show target, base, proposed version and publication action',async t=>{
 const h=harness(t);h.sandbox.sub='aiBoost';h.sandbox.data.aiBoost.status='ok';
 const item={id:'version-draft',type:'new_version',status:'submitted',revision:4,boosterId:'booster-id',baseVersionId:'base-id',version:'1.2.0',changelog:'<img src=x onerror=evil()>',content:{body:'# New content'},review:{title:'New version of Go',boosterName:'Go',baseVersion:'1.1.0'}};
 h.sandbox.api=async(path)=>{h.calls.push({path});return path.includes('?')?{items:[item]}:item};
 await h.run('loadBoostDrafts()');
 assert.match(h.dom.window.document.querySelector('#content').textContent,/New version of Go/);
 assert.match(h.dom.window.document.querySelector('#content').textContent,/1.1.0 → 1.2.0/);
 await h.run('boostPreview("version-draft")');
 assert.equal(h.calls[1].path,'ai-boost/drafts/version-draft');
 assert.equal(h.sandbox.dialog[0],'New version of Go');
 const doc=h.dom.window.document;
 assert.equal(doc.querySelector('[value="approve"]').textContent,'Publish new version');
 const details=doc.querySelector('.boost-review-scroll');
 for(const value of ['booster-id','base-id','1.1.0','1.2.0','Changelog']) assert.ok(details.textContent.includes(value));
 assert.equal(details.querySelector('img'),null);
 assert.doesNotMatch(details.textContent,/creates a private booster/);
});
test('decision locks displayed revision, blocks duplicate clicks and stays disabled after conflict',async t=>{
 const h=harness(t);let posts=[];let fail;
 h.sandbox.api=async(path,method,body)=>{
  if(method==='POST'){posts.push({path,body});return new Promise((resolve,reject)=>{fail=reject});}
  return {id:'one',name:'Review',status:'submitted',revision:7,content:{body:'body'}};
 };
 await h.run('boostPreview("one")');
 h.dom.window.document.querySelector('[name="feedback"]').value='ok';
 const first=submit(h,'approve');await submit(h,'reject');
 assert.equal(posts.length,1);assert.equal(h.sandbox.dialogBusy,true);
 assert.equal(h.dom.window.document.querySelector('#closeDialog').disabled,true);
 fail(new Error('Draft changed'));await first;
 assert.equal(posts[0].body.expectedRevision,7);assert.equal(posts[0].body.feedback,'ok');
 assert.equal(posts[0].path,'ai-boost/drafts/one/approve');
 assert.match(h.dom.window.document.querySelector('[role="alert"]').textContent,/Close and reopen/);
 assert.ok([...h.dom.window.document.querySelectorAll('.boost-review-decisions button')].every(b=>b.disabled));
 assert.equal(h.dom.window.document.querySelector('#closeDialog').disabled,false);
 await submit(h,'approve');assert.equal(posts.length,1);
});
test('decided preview is read-only; successful rejection closes modal and refreshes queue',async t=>{
 const h=harness(t);
 const item={id:'one',name:'Review',status:'converted',revision:2,content:{body:'body'}};
 h.sandbox.api=async()=>item;await h.run('boostPreview("one")');
 assert.match(h.sandbox.dialog[1],/already been decided/);
 assert.equal(h.dom.window.document.querySelector('[value="approve"]'),null);
 assert.equal(h.dom.window.document.querySelector('[value="reject"]'),null);
 item.status='submitted';await h.run('boostPreview("one")');
 const calls=[];h.sandbox.api=async(path,method,body)=>{calls.push({path,method,body});return {items:[]};};
 h.dom.window.document.querySelector('[name="feedback"]').value='Not useful';
 await submit(h,'reject');
 assert.equal(calls[0].path,'ai-boost/drafts/one/reject');
 assert.equal(calls[0].body.feedback,'Not useful');assert.equal(calls[0].body.expectedRevision,2);
 assert.match(calls[1].path,/drafts\?status=/);
 assert.equal(h.dom.window.document.querySelector('#boostReviewForm'),null);
 assert.equal(h.sandbox.notification,'Draft rejected.');
});
