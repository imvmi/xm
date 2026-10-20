const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const source=fs.readFileSync(path.join(__dirname,'../static/migrations.js'),'utf8');
const status=(id='job',state='completed',acknowledged=false)=>({
 database_driver:'sqlite',active_storage:'local',sources:[],maintenance:false,
 job:{id,kind:'media',source:'remote',target:'local',status:state,acknowledged,done:1,total:1,bytes:1,total_bytes:1,failed:0,failures:[],updated_at:1}
});
function fixture(api){
 const nodes=new Map();
 const node=id=>{
  if(!nodes.has(id))nodes.set(id,{value:'',hidden:false,dataset:{},listeners:{},addEventListener(type,fn){this.listeners[type]=fn;},removeAttribute(){},focus(){}});
  return nodes.get(id);
 };
 const ctx=vm.createContext({$:node,api,document:{querySelectorAll:()=>[]},state:{authenticated:true,view:'settings'},setInterval(){},escape:String,date:String,number:String,bytes:String,toast(){},action:(_,fn)=>fn()});
 vm.runInContext(source,ctx);
 return {node,refresh:()=>vm.runInContext('refreshMigrations()',ctx),ack:()=>node('migration-acknowledge').listeners.click()};
}
test('only confirmed completion hides the card; new and failed jobs remain visible',async()=>{
 let data=status();
 const f=fixture(async(url,method,body)=>{
  if(method==='POST'){assert.equal(url,'/migrations/acknowledge');assert.equal(body.id,'job');data=status('job','completed',true);return {message:'ok'};}
  return structuredClone(data);
 });
 await f.refresh();
 assert.equal(f.node('migration-job-panel').hidden,false);
 assert.equal(f.node('migration-acknowledge').hidden,false);
 await f.ack();
 assert.equal(f.node('migration-job-panel').hidden,true);
 await f.refresh();
 assert.equal(f.node('migration-job-panel').hidden,true);
 for(const state of ['running','switching','paused','failed','completed']){
  data=status('new-job',state);
  await f.refresh();
  assert.equal(f.node('migration-job-panel').hidden,false);
  assert.equal(f.node('migration-acknowledge').hidden,state!=='completed');
 }
});
test('a stale poll cannot reopen a confirmed completion',async()=>{
 let resolveOld,hold=false,acknowledged=false;
 const f=fixture(async(_,method)=>{
  if(method==='POST'){acknowledged=true;return {message:'ok'};}
  if(hold){hold=false;return new Promise(resolve=>resolveOld=resolve);}
  return status('job','completed',acknowledged);
 });
 await f.refresh();
 hold=true;
 const oldPoll=f.refresh();
 await f.ack();
 resolveOld(status());
 await oldPoll;
 assert.equal(f.node('migration-job-panel').hidden,true);
});
test('failed acknowledgement keeps the completion visible for retry',async()=>{
 const f=fixture(async(_,method)=>{if(method==='POST')throw Error('save failed');return status();});
 await f.refresh();
 await assert.rejects(f.ack(),/save failed/);
 assert.equal(f.node('migration-job-panel').hidden,false);
});
test('cleanup needs its own confirmation and sends a cleanup job without storage settings',async()=>{
 let submitted;
 const f=fixture(async(route,method,body)=>{
  if(route==='/migrations/preview'){assert.equal(body.kind,'cleanup');return {items:2,bytes:1024,required_bytes:0,free_bytes:4096,destination:'copies',warning:'verify before deleting'};}
  if(method==='POST'){submitted=body;return {};}
  return {...status(),local_copies:{items:2,bytes:1024}};
 });
 await f.refresh();
 assert.equal(f.node('migration-cleanup-preview').disabled,false);
 await f.node('migration-cleanup-preview').listeners.click();
 assert.equal(f.node('migration-confirm').placeholder,'清理本地副本');
 f.node('migration-confirm').value='开始迁移';
 f.node('migration-confirm').listeners.input();
 assert.equal(f.node('migration-start').disabled,true);
 f.node('migration-confirm').value='清理本地副本';
 f.node('migration-confirm').listeners.input();
 assert.equal(f.node('migration-start').disabled,false);
 await f.node('migration-start').listeners.click();
 assert.equal(submitted.kind,'cleanup');assert.equal(submitted.confirm,'清理本地副本');
 assert.equal(submitted.storage,undefined);assert.equal(submitted.database,undefined);
});
