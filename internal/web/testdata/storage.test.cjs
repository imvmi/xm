const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');

const source=fs.readFileSync(path.join(__dirname,'../static/storage.js'),'utf8');

function fixture(){
 const nodes=new Map(),calls=[];
 const node=id=>{
  if(!nodes.has(id))nodes.set(id,{id,hidden:false,checked:false,value:'',textContent:'',className:'',listeners:{},addEventListener(name,fn){this.listeners[name]=fn}});
  return nodes.get(id);
 };
 let storage={active:'remote',profiles:[{id:'remote',kind:'webdav',name:'NAS',endpoint:'https://storage.example'}],cache:{enabled:true,max_bytes:2147483648,used_bytes:1048576}};
 const api=async(route,method='GET',body)=>{calls.push({route,method,body});if(method==='PUT')storage.cache={...body,used_bytes:storage.cache.used_bytes};if(method==='DELETE')storage.cache.used_bytes=0;return route==='/storage'?storage:{ok:true}};
 const context=vm.createContext({$:node,api,hydrateSettingsForm:(_id,fill)=>fill(),acceptSettingsForm(){},settingsSnapshot:()=>'',bytes:n=>n+' B',toast(){},action:async(_button,work)=>work()});
 vm.runInContext(source,context);
 return {node,calls,context,setStorage:value=>storage=value};
}

test('remote storage exposes persistent cache controls and saves byte capacity',async()=>{
 const f=fixture();
 await f.context.refreshStorage();
 assert.equal(f.node('remote-cache-panel').hidden,false);
 assert.equal(f.node('remote-cache-note').hidden,false);
 assert.equal(f.node('remote-cache-enabled').checked,true);
 assert.equal(f.node('remote-cache-size').value,2048);
 f.node('remote-cache-enabled').checked=false;
 f.node('remote-cache-size').value='512';
 f.node('remote-cache-form').listeners.submit({preventDefault(){},submitter:{}});
 await new Promise(setImmediate);
 const update=f.calls.find(call=>call.method==='PUT');
 assert.equal(JSON.stringify(update.body),JSON.stringify({enabled:false,max_bytes:512*1024**2}));
 f.setStorage({active:'local',profiles:[],cache:{enabled:false,max_bytes:2147483648,used_bytes:0}});
 await f.context.refreshStorage();
 assert.equal(f.node('remote-cache-panel').hidden,true);
 assert.equal(f.node('remote-cache-note').hidden,true);
});

test('cache controls follow active storage, not saved profiles or existing cache',async()=>{
 const f=fixture();
 const profiles=[{id:'dav',kind:'webdav',name:'NAS'},{id:'s3',kind:'s3',name:'Bucket'}];
 const cache={enabled:true,max_bytes:2147483648,used_bytes:1048576};
 for(const active of ['local','dav','local','s3','local']){
  f.setStorage({active,profiles,cache});
  await f.context.refreshStorage();
  assert.equal(f.node('remote-cache-panel').hidden,active==='local',active);
  assert.equal(f.node('remote-cache-note').hidden,active==='local',active);
 }
 assert.equal(f.calls.every(call=>call.method==='GET'),true,'visibility must not modify saved cache settings or files');
});
