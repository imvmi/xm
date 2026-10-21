const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/accounts.js'),'utf8');
function fixture(ready=true){
 const controls={'archive-overview':{addEventListener(){}},'archive-overview-toggle':{},'user-search':{value:''},'monitor-filter':{value:'all'},'monitor-sort':{value:'username'}};
 const state={users:[
  {username:'alice',current_username:'alice_new',name:'Alice Name',monitored:true,posts:12,media:4,stats_pending:false},
  {username:'bob',name:'Bob',monitored:false,show_in_timeline:false,posts:5,media:2,stats_pending:false}
 ],monitors:[
  {username:'alice',current_username:'alice_new',enabled:true,show_in_timeline:true,posts:-1,last_finished:100},
  {username:'newbie',enabled:false,posts:-1,last_error:'failed',last_finished:200}
 ]};
 const escape=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
 const ctx=vm.createContext({state,monitorListReady:ready,document:{addEventListener(){}},window:{addEventListener(){}},$:id=>controls[id],handle:u=>u.current_username||u.username,visibleInTimeline:u=>u.show_in_timeline!==false,escape,number:String,date:String,avatar:()=>'',icon:()=>'',userURL:name=>'#user/'+name,userStat:(u,k)=>u[k]||0,monitorStatus:()=>['已暂停','neutral'],baselineHTML:()=>'<p>历史基线</p>',checkpointHTML:()=>'<p>断点</p>',monitorNextStep:()=>'',collectionActions:()=>'<button data-op="run">检查新动态</button>'});
 vm.runInContext(source.slice(0,source.indexOf('function accountCard(')),ctx);
 vm.runInContext(source.slice(source.indexOf('function accountCard('),source.indexOf('function renderAccounts(')),ctx);
 return {ctx,state,controls,rows:()=>ctx.accountRows(),names:()=>Array.from(ctx.filteredAccounts(ctx.accountRows()),u=>u.username)};
}
test('one row per archive key includes unmonitored archives and new empty monitors',()=>{
 const f=fixture(),rows=f.rows();
 assert.equal(rows.length,3);
 assert.equal(rows[0].posts,12);assert.equal(rows[0].media,4);assert.equal(rows[0].stats_pending,false);
 assert.equal(rows[1].monitored,false);assert.equal(rows[1].show_in_timeline,false);
 assert.equal(rows[2].monitored,true);assert.equal(rows[2].stats_pending,true);
});
test('directory remains useful before runtime loads, without pretending collection is healthy',()=>{
 const f=fixture(false),rows=f.rows();
 assert.equal(rows.length,2);assert.equal(rows[0].runtime_pending,true);
 assert.equal(rows[0].posts,12);assert.equal(rows[1].runtime_pending,false);
});
test('filters distinguish paused monitors from archive-only accounts and allow old/new/name search',()=>{
 const f=fixture();
 for(const [filter,want] of [['enabled',['alice']],['paused',['newbie']],['archived',['bob']],['attention',['newbie']],['hidden',['bob']]]){
  f.controls['monitor-filter'].value=filter;assert.deepEqual(f.names(),want);
 }
 f.controls['monitor-filter'].value='all';
 for(const q of ['alice','alice_new','Alice Name']){
  f.controls['user-search'].value=q;assert.deepEqual(f.names(),['alice']);
 }
});
test('pending statistics sort last in either direction, not as zero',()=>{
 const f=fixture();
 f.controls['monitor-sort'].value='posts-desc';assert.deepEqual(f.names(),['alice','bob','newbie']);
 f.controls['monitor-sort'].value='posts-asc';assert.deepEqual(f.names(),['bob','alice','newbie']);
 f.controls['monitor-sort'].value='recent';assert.deepEqual(f.names(),['newbie','alice','bob']);
});
test('cancelled monitors stay as archives; re-added rows retain counts and do not duplicate',()=>{
 const f=fixture();
 f.state.monitors=[];assert.equal(f.rows().length,2);assert.equal(f.rows()[0].monitored,false);
 f.state.monitors=[{username:'alice',enabled:true,posts:-1}];
 assert.equal(f.rows().length,2);assert.equal(f.rows()[0].monitored,true);assert.equal(f.rows()[0].posts,12);
});
test('cards expose independent switches, truncate names and move diagnostics into management',()=>{
 const f=fixture(),u={...f.rows()[0],last_error:'error <img>',name:'Very long "name"',enabled:false};
 const card=f.ctx.accountCard(u);
 assert.equal((card.match(/role="switch"/g)||[]).length,2);
 assert.match(card,/aria-checked="true"[^>]*data-op="timeline"/);
 assert.match(card,/aria-checked="false"[^>]*data-op="toggle"/);
 assert.ok(card.indexOf('data-op="timeline"')<card.indexOf('data-op="toggle"'));
 assert.match(card,/title="Very long &quot;name&quot;"/);
 assert.match(card,/需关注/);
 assert.match(card,/aria-haspopup="dialog"/);
 assert.doesNotMatch(card,/<details|data-op="edit"|error &lt;img&gt;/);
 vm.runInContext("accountExpanded.add('alice:status')",f.ctx);
 const menu=f.ctx.accountManagementHTML(u);
 assert.match(menu,/<details[^>]* open>/);
 assert.match(menu,/采集详情/);
 assert.match(menu,/error &lt;img&gt;/);
 assert.match(menu,/data-op="edit"/);
 assert.doesNotMatch(menu,/data-op="toggle"/);
});
test('archive-only and unresolved runtime accounts cannot toggle collection',()=>{
 const f=fixture();
 const archived=f.rows()[1];
 assert.match(f.ctx.accountCard(archived),/aria-checked="false"[^>]*data-op="toggle" disabled/);
 assert.match(f.ctx.accountManagementHTML(archived),/data-op="readd"/);
 assert.doesNotMatch(f.ctx.accountManagementHTML(archived),/采集详情|data-op="delete"/);
 const pending=fixture(false),u=pending.rows()[0];
 assert.match(pending.ctx.accountCard(u),/data-op="toggle" disabled/);
 assert.doesNotMatch(pending.ctx.accountManagementHTML(u),/data-op="edit"|data-op="run"/);
});
test('global pause remains distinct from an enabled per-account switch',()=>{
 const f=fixture();f.state.overview={paused:true};
 const u=f.rows()[0];
 assert.match(f.ctx.accountCard(u),/aria-checked="true"[^>]*data-op="toggle"/);
 assert.match(f.ctx.accountManagementHTML(u),/全局自动采集已暂停/);
});
