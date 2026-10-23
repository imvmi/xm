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
 const api=async(route,method='GET',body)=>{calls.push({route,method,body});if(method==='PUT'&&route==='/storage/cache')storage.cache={...body,used_bytes:storage.cache.used_bytes};if(method==='DELETE')storage.cache.used_bytes=0;return route==='/storage'?storage:{ok:true}};
 const context=vm.createContext({$:node,api,state:{authenticated:true},escape:s=>String(s??'').replace(/[&<>"']/g,'_'),hydrateSettingsForm:(_id,fill)=>fill(),acceptSettingsForm(){},settingsSnapshot:()=>'',bytes:n=>n+' B',toast(){},action:async(_button,work)=>work()});
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


test('credential rotation preserves destination and clears submitted secrets',async()=>{
 const f=fixture();
 f.setStorage({active:'local',profiles:[{id:'old-dav',kind:'webdav',name:'Old NAS',endpoint:'https://nas.example',username:'user'}],cache:{enabled:false,max_bytes:67108864,used_bytes:0}});
 await f.context.refreshStorage();
 assert.equal(f.node('storage-credentials-panel').hidden,false,'historical profiles remain repairable');
 assert.equal(f.node('storage-credentials-login').hidden,false);
 assert.equal(f.node('storage-credentials-username').value,'user');
 f.node('storage-credentials-password').value='replacement';
 f.node('storage-credentials-form').listeners.submit({preventDefault(){},submitter:{}});
 await new Promise(setImmediate);
 const request=f.calls.find(c=>c.route.endsWith('/credentials'));
 assert.equal(request.route,'/storage/profiles/old-dav/credentials');
 assert.equal(JSON.stringify(request.body),JSON.stringify({password:'replacement'}));
 assert.equal(f.node('storage-credentials-password').value,'');
});

test('S3 credentials support explicit token removal and profile switches discard secrets',async()=>{
 const f=fixture();
 f.setStorage({active:'s3',profiles:[{id:'s3',kind:'s3',name:'S3'},{id:'dav',kind:'webdav',name:'NAS',username:'user'}],cache:{enabled:false,max_bytes:67108864,used_bytes:0}});
 await f.context.refreshStorage();
 assert.equal(f.node('storage-credentials-s3').hidden,false);
 assert.equal(f.node('storage-credentials-login').hidden,true);
 f.node('storage-credentials-access').value='key';f.node('storage-credentials-secret').value='secret';f.node('storage-credentials-clear-token').checked=true;
 f.node('storage-credentials-form').listeners.submit({preventDefault(){},submitter:{}});
 await new Promise(setImmediate);
 const request=f.calls.find(c=>c.route.endsWith('/credentials'));
 assert.equal(JSON.stringify(request.body),JSON.stringify({access_key:'key',secret_key:'secret',session_token:''}));
 assert.equal(f.node('storage-credentials-secret').value,'');
 f.node('storage-credentials-token').value='draft';
 f.node('storage-credentials-profile').value='dav';f.node('storage-credentials-profile').listeners.change();
 assert.equal(f.node('storage-credentials-token').value,'');
 assert.equal(f.node('storage-credentials-login').hidden,false);
});
