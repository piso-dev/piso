const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const {JSDOM} = require('jsdom');
const script = fs.readFileSync(`${__dirname}/index.html`, 'utf8').match(/<script>([\s\S]*)<\/script>/)[1];
const slice = (a,b) => script.slice(script.indexOf(a),script.indexOf(b));
// Sandbox values carry the vm realm's prototypes, which deepStrictEqual rejects.
const json = (value) => JSON.parse(JSON.stringify(value));
function harness(t, telegram) {
  const dom = new JSDOM('<div id="content"></div><span id="resultCount"></span><div id="toast"></div><dialog id="dialog"><h2 id="dialogTitle"></h2><button id="closeDialog"></button><div id="dialogContent"></div></dialog>',{url:'http://piso.local'});
  t.after(()=>dom.window.close());
  const calls=[];
  const sandbox={document:dom.window.document,window:dom.window,location:dom.window.location,URL,count:()=>{},sub:'telegram',query:'',loaded:new Set(['telegram']),
    data:{telegram},
    $:s=>dom.window.document.querySelector(s), matches:()=>true,list:()=>[],hideProject:false,ingressPort:80,
    api:async(path,method,body)=>{calls.push({path,method,body});return {botUsername:'piso_bot'}},
    changed:async(keys)=>{sandbox.changedKeys=keys},refresh:async()=>{},action:async()=>{},errorDialog:e=>{throw e},safeURL:s=>s,
    dialogBusy:false, notify:message=>{sandbox.notification=message},closeDialog:()=>{dom.window.document.querySelector("#dialogContent").innerHTML=""},
    confirmAction:(...args)=>{sandbox.confirmation=args},formDialog:(...args)=>{sandbox.form=args},
  };
  const ctx=new Proxy(sandbox,{has:()=>true,get:(o,k)=>k in o?o[k]:globalThis[k]});vm.createContext(ctx);
  const helpers=[
    slice('const esc =','const endpoints'),
    slice('function badge(','function empty('),slice('function empty(','function record('),
    slice('function record(','function routeURL('),slice('function button(','function facts('),slice('function facts(','function notify('),
    slice('function replaceContent(','function count('),slice('function count(','function mountDialog('),
    slice('const intros = {','function renderSettings()'),slice('function renderSettings()','async function changed('),
    slice('function field(','function formDialog(')
  ].join('\n');
  vm.runInContext(helpers,ctx);
  return {dom,sandbox,ctx,calls,run:s=>vm.runInContext(s,ctx)};
}
test('Telegram tab explains the connect flow before a bot is connected',t=>{
 const h=harness(t,{connected:false});h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/No Telegram bot connected/);
 assert.ok(h.dom.window.document.querySelector('[data-action="telegram-connect"]'));
 assert.equal(h.dom.window.document.querySelector('[data-action="telegram-disconnect"]'),null);
});
test('Telegram tab shows the connected bot and its public link',t=>{
 const h=harness(t,{connected:true,botId:42,botUsername:'piso_bot',createdAt:'2024-05-01T10:00:00Z'});h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/@piso_bot/);
 assert.ok(h.dom.window.document.querySelector('a[href="https://t.me/piso_bot"]'));
 assert.ok(h.dom.window.document.querySelector('[data-action="telegram-disconnect"]'));
 assert.match(h.dom.window.document.body.textContent,/Reconnect/);
});
// The credential is present but Telegram is not answering: stay connected and
// surface the failure instead of pretending the token vanished.
test('Telegram tab stays connected when the bot cannot be verified',t=>{
 const h=harness(t,{connected:true,warning:'Telegram is unreachable right now.'});h.run('renderSettings()');
 assert.match(h.dom.window.document.body.textContent,/Telegram is unreachable right now\./);
 assert.match(h.dom.window.document.body.textContent,/Not polling/);
 assert.equal(h.dom.window.document.querySelector('a[href^="https://t.me/"]'),null);
});
// The token is stored, so the bot is connected even though nobody may talk to
// it yet. The empty state has to say so rather than look like a failure.
test('Telegram tab denies everyone until an account is claimed',t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,users:[]});h.run('renderSettings()');
 const text=h.dom.window.document.body.textContent;
 assert.match(text,/Nobody can talk to this bot yet/);
 assert.match(text,/empty list means deny/);
 assert.match(text,/Get claim code/);
 assert.match(text,/Connected/);
 assert.equal(h.dom.window.document.querySelector('[data-action="telegram-revoke"]'),null);
});
// The code is read off the screen and typed into Telegram, so the grouping is
// presentation only and the expiry has to be visible.
test('a pending claim code is shown grouped with its expiry',t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,claimCode:'ABCD2345',claimExpiresAt:'2024-05-01T10:00:00Z',users:[]});h.run('renderSettings()');
 const text=h.dom.window.document.body.textContent;
 assert.match(text,/Waiting for a claim/);
 assert.match(text,/\/claim ABCD-2345/);
 assert.match(text,/Expires/);
 assert.ok(h.dom.window.document.querySelector('[data-action="telegram-claim"]'));
});
test('allowed accounts are listed with a revoke action',t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,users:[{userId:500,chatId:500,username:'ada',firstName:'Ada',claimedAt:'2024-05-01T10:00:00Z'}]});h.run('renderSettings()');
 const text=h.dom.window.document.body.textContent;
 assert.match(text,/Ada/);
 assert.match(text,/@ada/);
 assert.match(text,/Telegram ID/);
 assert.match(text,/Add person/);
 const revoke=h.dom.window.document.querySelector('[data-action="telegram-revoke"]');
 assert.equal(revoke.dataset.id,'500');
});
test('a running poller reports the last poll',t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,lastPoll:'2024-05-01T10:00:00Z'});h.run('renderSettings()');
 const text=h.dom.window.document.body.textContent;
 assert.match(text,/Last poll/);
 assert.doesNotMatch(text,/Not polling/);
});
test('getting a claim code posts and surfaces the grouped code',async t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,users:[]});
 h.sandbox.api=async(path,method,body)=>{h.calls.push({path,method,body});return {code:'ABCD2345'}};
 await h.run('telegramClaim()');
 assert.deepEqual(json(h.calls),[{path:'telegram/claim',method:'POST',body:{}}]);
 assert.deepEqual(json(h.sandbox.changedKeys),['telegram']);
 assert.match(h.sandbox.notification,/\/claim ABCD-2345/);
});
test('revoking confirms before deleting the account',async t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot',running:true,users:[{userId:500,chatId:500}]});
 h.run('telegramRevoke(500)');
 const [title,description,label,task]=h.sandbox.confirmation;
 assert.equal(label,'Revoke');
 assert.match(description,/stops reaching this gateway/);
 await task();
 assert.deepEqual(json(h.calls),[{path:'telegram/users/500',method:'DELETE'}]);
 assert.deepEqual(json(h.sandbox.changedKeys),['telegram']);
});
test('connecting posts the trimmed token and refreshes the tab',async t=>{
 const h=harness(t,{connected:false});
 h.run('telegramConnect()');
 const [title,description,fields,submit,onSubmit]=h.sandbox.form;
 assert.equal(title,'Connect to Telegram');
 assert.match(fields,/name="token"/);
 assert.match(fields,/type="password"/);
 assert.equal(submit,'Connect');
 const message=await onSubmit({token:'  123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw  '});
 assert.deepEqual(json(h.calls),[{path:'telegram',method:'POST',body:{token:'123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw'}}]);
 assert.deepEqual(json(h.sandbox.changedKeys),['telegram']);
 assert.equal(message,'Connected to @piso_bot.');
});
test('reconnecting keeps the BotFather instructions and relabels the action',t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot'});
 h.run('telegramConnect()');
 const [title,description,fields,submit]=h.sandbox.form;
 assert.equal(title,'Reconnect Telegram');
 assert.equal(submit,'Replace token');
 assert.match(fields,/BotFather/);
 assert.match(description,/never exported to a worker/);
});
test('disconnecting confirms before deleting the token',async t=>{
 const h=harness(t,{connected:true,botUsername:'piso_bot'});
 h.run('telegramDisconnect()');
 const [title,description,label,task]=h.sandbox.confirmation;
 assert.equal(label,'Disconnect');
 assert.match(description,/stays on Telegram/);
 await task();
 assert.deepEqual(json(h.calls),[{path:'telegram',method:'DELETE'}]);
 assert.deepEqual(json(h.sandbox.changedKeys),['telegram']);
});
