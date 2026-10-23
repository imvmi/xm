const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const calls=[],timers=new Map();let timerID=0,logins=0;
 const ctx=vm.createContext({AbortController,DOMException,state:{csrf:'csrf'},
  setTimeout:fn=>{timers.set(++timerID,fn);return timerID},clearTimeout:id=>timers.delete(id),
  showLogin:()=>{logins++;ctx.cancelPostReads()},
  fetch:(url,options)=>new Promise((resolve,reject)=>{
   const abort=()=>reject(new DOMException('cancelled','AbortError'));
   if(options.signal.aborted)abort();else options.signal.addEventListener('abort',abort,{once:true});
   calls.push({url,options,resolve,reject});
  })});
 vm.runInContext(source.slice(source.indexOf('const postReadControllers='),source.indexOf('async function action(')),ctx);
 return {ctx,calls,timers,logins:()=>logins,expire(){for(const fn of [...timers.values()])fn()}};
}
const response=(body={},status=200)=>({ok:status<400,status,json:async()=>body});
test('obsolete post, reply and context reads abort together; mutations remain independent',async()=>{
 const f=fixture();
 const reads=['/posts?q=old','/posts/1','/posts/1/context','/posts/1/replies'].map(p=>f.ctx.api(p));
 const rejected=reads.map(p=>assert.rejects(p,{name:'AbortError'}));
 const write=f.ctx.api('/posts/1/bookmark','PUT',{bookmarked:true});
 f.ctx.cancelPostReads();await Promise.all(rejected);
 assert.ok(f.calls.slice(0,4).every(r=>r.options.signal.aborted));
 assert.equal(f.calls[4].options.signal.aborted,false);
 f.calls[4].resolve(response());await write;assert.equal(f.timers.size,0);
});
test('client timeout covers stalled headers and clears its timer',async()=>{
 const f=fixture(),request=f.ctx.api('/posts');
 const failed=assert.rejects(request,{name:'TimeoutError',message:'请求超时，请稍后重试'});
 f.expire();await failed;assert.equal(f.calls[0].options.signal.aborted,true);assert.equal(f.timers.size,0);
});
test('client timeout covers stalled JSON response bodies',async()=>{
 const f=fixture(),request=f.ctx.api('/posts');
 const failed=assert.rejects(request,{name:'TimeoutError'});
 f.calls[0].resolve({ok:true,status:200,json:()=>new Promise((resolve,reject)=>f.calls[0].options.signal.addEventListener('abort',()=>reject(new DOMException('body aborted','AbortError'))))});
 await new Promise(setImmediate);f.expire();await failed;assert.equal(f.timers.size,0);
});
test('explicit cancellation stays distinct from network errors and server deadlines',async()=>{
 const f=fixture(),controller=new AbortController();
 const request=f.ctx.api('/posts','GET',undefined,{signal:controller.signal});
 const failed=assert.rejects(request,{name:'AbortError'});controller.abort();await failed;
 const network=f.ctx.api('/posts');f.calls[1].reject(new Error('offline'));
 await assert.rejects(network,/无法连接服务/);
 const deadline=f.ctx.api('/posts');f.calls[2].resolve(response({error:'查询超时，请缩小搜索范围后重试'},504));
 await assert.rejects(deadline,/查询超时/);assert.equal(f.timers.size,0);
});
test('completed requests detach abort listeners and do not affect later reads',async()=>{
 const f=fixture(),controller=new AbortController();let removed=0;
 const original=controller.signal.removeEventListener.bind(controller.signal);
 controller.signal.removeEventListener=(...args)=>{removed++;original(...args)};
 const first=f.ctx.api('/posts','GET',undefined,{signal:controller.signal});
 f.calls[0].resolve(response({items:[]}));await first;assert.equal(removed,1);assert.equal(f.timers.size,0);
 controller.abort();assert.equal(f.calls[0].options.signal.aborted,false);
 const next=f.ctx.api('/posts');f.calls[1].resolve(response({items:[]}));await next;
 f.ctx.cancelPostReads();assert.equal(f.calls[1].options.signal.aborted,false);
});
test('expired login preserves its message while cancelling other post reads',async()=>{
 const f=fixture(),first=f.ctx.api('/posts'),other=f.ctx.api('/posts/1/context');
 const canceled=assert.rejects(other,{name:'AbortError'});
 f.calls[0].resolve(response({error:'login'},401));await assert.rejects(first,/登录已过期/);await canceled;
 assert.equal(f.logins(),1);assert.equal(f.timers.size,0);
});
