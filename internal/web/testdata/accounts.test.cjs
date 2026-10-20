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
 const ctx=vm.createContext({state,monitorListReady:ready,document:{addEventListener(){}},window:{addEventListener(){}},$:id=>controls[id],handle:u=>u.current_username||u.username,visibleInTimeline:u=>u.show_in_timeline!==false});
 vm.runInContext(source.slice(0,source.indexOf('function accountCard(')),ctx);
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
