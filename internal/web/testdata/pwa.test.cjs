const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '../static/sw.js'), 'utf8');

function fixture() {
  const events = {}, deleted = [], added = [];
  let claimed = false, skipped = false;
  const offline = new Response('offline screen');
  const cache = {addAll: async requests => added.push(...requests), match: async () => offline};
  const self = {location: {origin:'https://xm.test'}, clients:{claim:async()=>{claimed=true}},
    skipWaiting:()=>{skipped=true}, addEventListener:(name,fn)=>{events[name]=fn}};
  const ctx = vm.createContext({URL, Request, Response, AbortController, setTimeout, clearTimeout, self,
    caches: {open:async()=>cache, keys:async()=>['xm-pwa-old','xm-pwa-__XM_REVISION__','unrelated-cache'], delete:async name=>deleted.push(name)},
    fetch:async()=>new Response('network')});
  // Browser Request resolves relative URLs against the origin, Node does not.
  ctx.Request = class extends Request {constructor(url, options) {super(new URL(url, self.location.origin), options)}};
  vm.runInContext(source,ctx);
  function fetchEvent(url, extra={}) {
    let result;
    events.fetch({request:{url:new URL(url,self.location.origin).href, method:'GET',mode:'cors',...extra},respondWith:p=>{result=p}});
    return result;
  }
  return {ctx,events,deleted,added,fetchEvent,claimed:()=>claimed,skipped:()=>skipped};
}
test('installation caches only public recovery resources; activation keeps unrelated caches',async()=>{
  const f=fixture(); let pending;
  f.events.install({waitUntil:p=>{pending=p}}); await pending;
  assert.ok(f.added.some(r=>new URL(r.url).pathname==='/offline.html'));
  assert.ok(f.added.every(r=>r.cache==='reload' && /^\/(offline.html|assets\/(offline\.(css|js)|favicon.svg|.*\.png))$/.test(new URL(r.url).pathname)));
  f.events.activate({waitUntil:p=>{pending=p}}); await pending;
  assert.deepEqual(f.deleted,['xm-pwa-old']); assert.equal(f.claimed(),true);
  assert.equal(f.skipped(),false);
  f.events.message({data:{type:'SKIP_WAITING'}}); assert.equal(f.skipped(),true);
});
test('private requests, mutations, external requests and arbitrary assets bypass caches',()=>{
  const f=fixture();
  for(const url of ['/api/session','/api/posts','/api/settings','/media/1/1','/avatars/alice','/assets/index.html','/assets/app.js','https://external.test/']) {
    assert.equal(f.fetchEvent(url),undefined,url);
  }
  assert.equal(f.fetchEvent('/',{method:'POST',mode:'navigate'}),undefined);
  assert.equal(f.fetchEvent('/missing',{mode:'navigate'}),undefined);
});
test('navigation stays live; only network errors and server outages show recovery',async()=>{
  const f=fixture();
  assert.equal(await (await f.fetchEvent('/',{mode:'navigate'})).text(),'network');
  f.ctx.fetch=async()=>{throw new TypeError('offline')};
  assert.equal(await (await f.fetchEvent('/?source=pwa',{mode:'navigate'})).text(),'offline screen');
  const g=fixture();
  g.ctx.fetch=async()=>new Response('maintenance',{status:503});
  assert.equal(await (await g.fetchEvent('/',{mode:'navigate'})).text(),'offline screen');
  g.ctx.fetch=async()=>new Response('unauthorized',{status:401});
  assert.equal((await g.fetchEvent('/',{mode:'navigate'})).status,401);
});
test('a stalled navigation is aborted so the recovery screen remains reachable',async()=>{
  const f=fixture(); let expire, cleared=false, signal;
  f.ctx.setTimeout=(fn,ms)=>{assert.equal(ms,8000);expire=fn;return 1};
  f.ctx.clearTimeout=()=>{cleared=true};
  f.ctx.fetch=(_request,options)=>new Promise((_resolve,reject)=>{
    signal=options.signal;
    signal.addEventListener('abort',()=>reject(new Error('timeout')));
  });
  const response=f.fetchEvent('/',{mode:'navigate'});
  expire();
  assert.equal(await (await response).text(),'offline screen');
  assert.equal(signal.aborted,true); assert.equal(cleared,true);
});
