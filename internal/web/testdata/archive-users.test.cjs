const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const nodes=new Map(),requests=[],renders=[],errors=[];
 const $=id=>{if(!nodes.has(id))nodes.set(id,{hidden:false,textContent:'',setAttribute(){}});return nodes.get(id)};
 const state={authenticated:true,view:'archive',navRequest:1,users:[]};
 const ctx=vm.createContext({state,$,URLSearchParams,Date,number:String,toast:m=>errors.push(m),renderUsers:()=>renders.push(state.users.map(u=>({...u}))),renderProfile:()=>renders.push(state.users.map(u=>({...u}))),api:query=>new Promise((resolve,reject)=>requests.push({query,resolve,reject}))});
 vm.runInContext(source.slice(source.indexOf('let userDirectoryCache='),source.indexOf('function renderUsers()')),ctx);
 const directory=(names)=>names.map(username=>({username,name:username,stats_pending:true}));
 const stats=(names,n=5)=>names.map(username=>({username,posts:n,media:2,last_saved:'today'}));
 return {ctx,state,$,requests,renders,errors,directory,stats};
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));

test('directory renders and resolves before statistics; placeholders are not zeros',async()=>{
 const f=fixture(),pending=f.ctx.refreshUsers();
 assert.equal(f.requests[0].query,'/users?stats=0');
 f.requests[0].resolve(f.directory(['alice','bob']));await pending;
 assert.equal(f.state.users.length,2);assert.equal(f.ctx.userStat(f.state.users[0],'posts'),'—');
 assert.equal(f.requests[1].query,'/users/stats');
 f.requests[1].resolve(f.stats(['alice','bob']));await tick();
 assert.equal(f.ctx.userStat(f.state.users[0],'posts'),'5');assert.equal(f.$('user-status').hidden,true);
});
test('returning from an account immediately restores the full cached directory',async()=>{
 const f=fixture();let p=f.ctx.refreshUsers();f.requests[0].resolve(f.directory(['alice','bob']));await p;
 f.requests[1].resolve(f.stats(['alice','bob']));await tick();
 f.state.view='user';f.state.navRequest++;p=f.ctx.refreshUsers('alice');
 f.requests[2].resolve(f.directory(['alice']));await p;f.requests[3].resolve(f.stats(['alice']));await tick();
 f.state.view='archive';f.state.navRequest++;p=f.ctx.refreshUsers();
 assert.equal(f.state.users.length,2);assert.equal(f.state.users[1].posts,5);
 f.requests[4].resolve(f.directory(['alice','bob']));await p;f.requests[5].resolve(f.stats(['alice','bob']));await tick();
});
test('late directory and stats cannot replace another account or a logged-out view',async()=>{
 const f=fixture(),old=f.ctx.refreshUsers();
 f.state.navRequest++;f.state.view='user';const current=f.ctx.refreshUsers('bob');
 f.requests[1].resolve(f.directory(['bob']));await current;
 f.requests[0].resolve(f.directory(['alice']));await old;
 assert.equal(f.state.users[0].username,'bob');assert.equal(f.requests.length,3);
 f.state.authenticated=false;f.state.navRequest++;f.state.users=[];
 f.requests[2].resolve(f.stats(['bob']));await tick();assert.equal(f.state.users.length,0);
});
test('older statistics cannot overwrite a newer manual refresh',async()=>{
 const f=fixture();let p=f.ctx.refreshUsers();f.requests[0].resolve(f.directory(['alice']));await p;
 p=f.ctx.refreshUsers('',true);f.requests[2].resolve(f.directory(['alice']));await p;
 assert.equal(f.requests[3].query,'/users/stats?refresh=1');
 f.requests[3].resolve(f.stats(['alice'],10));await tick();
 f.requests[1].resolve(f.stats(['alice'],1));await tick();assert.equal(f.state.users[0].posts,10);
});
test('statistics failure preserves usable cards and cached counts with a retry hint',async()=>{
 const f=fixture();let p=f.ctx.refreshUsers();f.requests[0].resolve(f.directory(['alice']));await p;
 f.requests[1].resolve(f.stats(['alice']));await tick();
 p=f.ctx.refreshUsers('',true);f.requests[2].resolve(f.directory(['alice']));await p;
 f.requests[3].reject(new Error('offline'));await tick();
 assert.equal(f.state.users[0].posts,5);assert.match(f.$('user-status').textContent,/刷新重试/);assert.equal(f.errors.length,1);
});
test('directory failure keeps cached cards instead of displaying the empty archive message',async()=>{
 const f=fixture();let p=f.ctx.refreshUsers();f.requests[0].resolve(f.directory(['alice']));await p;
 f.requests[1].resolve(f.stats(['alice']));await tick();p=f.ctx.refreshUsers();
 f.requests[2].reject(new Error('offline'));await assert.rejects(p,/offline/);
 assert.equal(f.state.users[0].username,'alice');assert.equal(f.$('user-empty').hidden,true);assert.match(f.$('user-status').textContent,/加载失败/);
});
test('background polling does not duplicate an in-flight statistics request',async()=>{
 const f=fixture(),p=f.ctx.refreshUsers();f.requests[0].resolve(f.directory(['alice']));await p;
 await f.ctx.refreshUserStats();assert.equal(f.requests.length,2);
 f.requests[1].resolve(f.stats(['alice']));await tick();
 const next=f.ctx.refreshUserStats();assert.equal(f.requests.length,3);
 f.requests[2].resolve(f.stats(['alice'],6));await next;assert.equal(f.state.users[0].posts,6);
});
