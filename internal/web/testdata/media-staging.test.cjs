const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');

test('staging cleanup requires explicit confirmation and refreshes usage',async()=>{
 const nodes=new Map();
 const $=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',open:false,events:{},addEventListener(name,fn){this.events[name]=fn},showModal(){this.open=true},close(){this.open=false}});return nodes.get(id)};
 const calls=[];let pending;
 const ctx=vm.createContext({$,bytes:String,action:(node,fn)=>pending=fn(),api:async(url,method,body)=>{
  calls.push({url,method,body});
  return method==='POST'?{message:'已清理',removed_bytes:3}:{usage:{bytes:8,partial_bytes:3,complete_bytes:5},limit_bytes:100};
 }});
 vm.runInContext(source.slice(source.indexOf('async function refreshMediaStaging'),source.indexOf("$('reset-rate-limit').addEventListener")),ctx);
 $('media-staging-clear').events.click();assert.equal(calls.length,0);assert.equal($('media-staging-dialog').open,true);
 $('media-staging-confirm').events.click();await pending;
 assert.equal(calls[0].url,'/media/staging/cleanup');assert.equal(calls[0].body.confirm,true);assert.equal(calls[1].url,'/media/staging');
 assert.equal($('media-staging-dialog').open,false);assert.match($('media-staging-result').textContent,/已释放 3/);
 assert.match($('media-staging-usage').textContent,/完整待处理文件 5/);
});
