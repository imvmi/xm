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

test('proactive budget waiting is not displayed as collection failure',async()=>{
 const node={innerHTML:''};
 const ctx=vm.createContext({$:()=>node,escape:String,date:String,api:async()=>[
  {target:'alice',status:'budget_wait',posts:20,new_posts:2,message:'共享请求预算接近上限'}
 ]});
 vm.runInContext(source.slice(source.indexOf('const runLabels='),source.indexOf("$('refresh-runs').addEventListener")),ctx);
 await ctx.refreshRuns();
 assert.match(node.innerHTML,/预算等待/);
 assert.doesNotMatch(node.innerHTML,/采集失败/);
});
