const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const {JSDOM} = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a,b) => script.slice(script.indexOf(a),script.indexOf(b));
function harness(t) {
 const dom = new JSDOM('<div id="content"></div><span id="resultCount"></span><div id="toast"></div>',{url:'http://piso.local'});
 t.after(()=>dom.window.close());
 const calls=[];
 const sandbox={document:dom.window.document,window:dom.window,location:dom.window.location,URL,count:()=>{},sub:'connections',query:'',loaded:new Set(['connections']),
  data:{connections:{connections:[{id:'ai-boost',name:'AI Boost',status:'ok',detail:'Connected · worker suggestions and dashboard review'}]},aiBoost:{configured:true}},
  $:s=>dom.window.document.querySelector(s), matches:()=>true,list:()=>[],hideProject:false,ingressPort:80,
  api:async(path,method,body)=>{calls.push({path,method,body});return {url:'https://piso.example/oauth/ai-boost/start?ticket=one'}},
  changed:async()=>{},refresh:async()=>{},action:async()=>{},errorDialog:e=>{throw e},safeURL:s=>s,
  confirmAction:(...args)=>{sandbox.confirmation=args},formDialog:(...args)=>{sandbox.form=args},mountDialog:(...args)=>{sandbox.dialog=args},
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
test('Connections renders one-click connect, reconnect and disconnect controls',t=>{
 const h=harness(t);h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Connected · worker suggestions and dashboard review/);
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-authorize"]'));
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-disconnect"]'));
 h.sandbox.data.aiBoost.configured=false;h.run('renderSettings()');
 assert.ok(h.dom.window.document.querySelector('[data-action="boost-authorize"]'));
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-config"]'),null);
 assert.match(h.dom.window.document.body.textContent,/Connect AI Boost/);
 assert.doesNotMatch(h.dom.window.document.body.textContent,/Callback URL|API origin|Configure AI Boost/);
 h.sandbox.data.aiBoost.configured=true;h.sandbox.data.connections.connections[0].status='needs-auth';h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Reconnect AI Boost/);
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
 await h.run('loadBoostSuggestions()');
 assert.equal(h.dom.window.document.querySelector('img'),null);
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-review-page"]').dataset.id,'opaque');
 h.sandbox.api=async()=>{throw new Error('Queue unavailable')};
 await h.run('loadBoostSuggestions()');
 assert.match(h.dom.window.document.querySelector('[role="alert"]').textContent,/Queue unavailable/);
 assert.equal(h.dom.window.document.querySelector('[data-action="boost-preview"]'),null);
});
test('preview escapes content and locks decision to displayed revision, never auto-retries conflicts',async t=>{
 const h=harness(t);
 const item={id:'one',name:'Review',status:'submitted',revision:7,content:{body:'<script>evil()</script>'},destination:{name:'Library',tagId:'tag-uuid',visibility:'private'}};
 let posts=[];
 h.sandbox.api=async(path,method,body)=>{if(method==='POST'){posts.push({path,body});throw new Error('Suggestion changed');}return item;};
 await h.run('boostPreview("one")');
 assert.match(h.sandbox.form[2],/tag-uuid/);assert.match(h.sandbox.form[2],/private/);
 assert.match(h.sandbox.form[2],/&lt;script&gt;/);assert.doesNotMatch(h.sandbox.form[2],/<script>/);
 const submit=h.sandbox.form[4];
 await assert.rejects(submit({decision:'approve',feedback:'ok'}),/Suggestion changed/);
 assert.equal(posts[0].body.expectedRevision,7);
 assert.equal(posts[0].path,'ai-boost/suggestions/one/approve');
 await assert.rejects(submit({decision:'approve',feedback:'ok'}),/Close and reopen/);
 assert.equal(posts.length,1);
});
test('approved preview is read-only; successful rejection refreshes queue',async t=>{
 const h=harness(t);
 const item={id:'one',name:'Review',status:'converted',revision:2,content:{body:'body'}};
 h.sandbox.api=async()=>item;await h.run('boostPreview("one")');
 assert.match(h.sandbox.dialog[1],/already been decided/);assert.equal(h.sandbox.form,undefined);
 item.status='submitted';await h.run('boostPreview("one")');
 const calls=[];h.sandbox.api=async(path,method,body)=>{calls.push({path,method,body});return {items:[]};};
 assert.equal(await h.sandbox.form[4]({decision:'reject',feedback:'Not useful'}),'Suggestion rejected.');
 assert.equal(calls[0].path,'ai-boost/suggestions/one/reject');assert.match(calls[1].path,/suggestions\?status=/);
});
