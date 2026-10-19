const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');

test('run history distinguishes repeated reads from new posts and unknown legacy counts',async()=>{
 const node={innerHTML:''};
 const ctx=vm.createContext({$:()=>node,escape:String,date:String,api:async()=>[
  {target:'alice',status:'failed',posts:1006,new_posts:0,message:'rate'},
  {target:'bob',status:'bounded_complete',posts:20,new_posts:3,message:''},
  {target:'legacy',status:'failed',posts:806,new_posts:null,message:'rate'}
 ]});
 vm.runInContext(source.slice(source.indexOf('const runLabels='),source.indexOf("$('refresh-runs').addEventListener")),ctx);
 await ctx.refreshRuns();
 assert.match(node.innerHTML,/本轮读取 1006 条 · 新增 0 条/);
 assert.match(node.innerHTML,/本轮读取 20 条 · 新增 3 条/);
 assert.match(node.innerHTML,/本轮读取 806 条 · 新增未记录/);
});

test('legacy budget-wait history remains recognizable after removing the limiter',async()=>{
 const node={innerHTML:''};
 const ctx=vm.createContext({$:()=>node,escape:String,date:String,api:async()=>[
  {target:'alice',status:'budget_wait',posts:20,new_posts:2,message:'旧版本请求预算等待'}
 ]});
 vm.runInContext(source.slice(source.indexOf('const runLabels='),source.indexOf("$('refresh-runs').addEventListener")),ctx);
 await ctx.refreshRuns();
 assert.match(node.innerHTML,/历史等待/);
 assert.doesNotMatch(node.innerHTML,/采集失败/);
});

test('task center renders current work, FIFO positions, wait reason, and run history',async()=>{
 const nodes=Object.fromEntries(['current-job-count','queue-count','current-job','job-queue','queue-wait','runs'].map(id=>[id,{innerHTML:'',textContent:'',hidden:true}]));
 const ctx=vm.createContext({$:id=>nodes[id],escape:String,date:String,number:String,icon:name=>`ICON:${name}`,api:async()=>({
  current:{target:'alice',status:'running',source:'manual',started_at:10,attempts:1,posts_read:8,new_posts:2,phase:'media',elapsed_seconds:65,remaining_seconds:7200,media_done:3,media_total:8},
  queue:[{id:42,target:'bob',status:'waiting_rate',source:'automatic',queued_at:9,position:1,message:'限流冷却'}],
  runs:[{target:'carol',status:'bounded_complete',posts:4,new_posts:1,message:'',source:'manual',started_at:'start',finished_at:'end'}],
  wait_reason:'X 请求限流，队列将在冷却结束后继续',wait_until:20
 })});
 vm.runInContext(source.slice(source.indexOf('const runLabels='),source.indexOf("$('refresh-runs').addEventListener")),ctx);
 await ctx.refreshRuns();
 assert.match(nodes['current-job'].innerHTML,/@alice/);
 assert.match(nodes['current-job'].innerHTML,/已读取 8 条/);
 assert.match(nodes['current-job'].innerHTML,/下载与归档媒体/);
 assert.match(nodes['current-job'].innerHTML,/已运行 1 分 5 秒/);
 assert.match(nodes['current-job'].innerHTML,/时限剩余 2 小时 0 分钟/);
 assert.match(nodes['current-job'].innerHTML,/3 \/ 8/);
 assert.match(nodes['current-job'].innerHTML,/停止并暂停监控/);
 assert.match(nodes['job-queue'].innerHTML,/@bob/);
 assert.match(nodes['job-queue'].innerHTML,/waiting_rate|限流冷却/);
 assert.match(nodes['job-queue'].innerHTML,/data-cancel-job="42"/);
 assert.match(nodes.runs.innerHTML,/@carol/);
 assert.equal(nodes['queue-wait'].hidden,false);
 assert.match(nodes['queue-wait'].textContent,/请求限流/);
});
