const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');

function fixture(){
 const nodes=new Map(),calls=[],toasts=[];
 const $=id=>{if(!nodes.has(id))nodes.set(id,{value:'',textContent:'',disabled:false,hidden:false,
  setAttribute(){},reportValidity:()=>true,classList:{add(){},remove(){},toggle(){}}});return nodes.get(id)};
 $('browser-mode').value='local';$('cdp-host').value='127.0.0.1';$('cdp-port').value='5818';
 const ctx=vm.createContext({$,AbortController,state:{settings:{browser_mode:'local',chrome_headless:true}},
  isRequestAbort:err=>err.name==='AbortError',toast:message=>toasts.push(message),
  api:(path,method,data,options)=>new Promise((resolve,reject)=>calls.push({path,method,data,options,resolve,reject}))});
 vm.runInContext('let chromeModePending=false,chromeDiagnosticPending=false,chromeDiagnosticController=null;'+
  source.slice(source.indexOf('function renderChromeMode(){'),source.indexOf("$('chrome-mode').addEventListener"))+
  source.slice(source.indexOf('function syncBrowserFields(){'),source.indexOf('function populateBrowserSettings'))+
  source.slice(source.indexOf('function browserSettingsDraft(){'),source.indexOf("$('browser-settings-form').addEventListener('submit'"))+
  source.slice(source.indexOf('function cancelBrowserDiagnostic(){'),source.indexOf("$('test-browser-connection').addEventListener")),ctx);
 return {ctx,$,calls,toasts};
}
const response={ok:true,steps:[{status:'passed',name:'Chrome',message:'fixture'}],latency_ms:5};

test('headless controls follow unsaved local/CDP selection in both directions',()=>{
 const f=fixture();f.$('browser-mode').value='cdp';f.ctx.syncBrowserFields();
 assert.equal(f.$('chrome-mode').disabled,true);assert.equal(f.$('chrome-mode-state').textContent,'远端管理');
 assert.equal(f.$('cdp-fields').hidden,false);
 f.ctx.state.settings.browser_mode='cdp';f.$('browser-mode').value='local';f.ctx.syncBrowserFields();
 assert.equal(f.$('chrome-mode').disabled,false);assert.equal(f.$('chrome-mode-state').textContent,'Headless');
 assert.equal(f.$('chrome-mode-help').hidden,false);assert.equal(f.$('cdp-host').disabled,true);
});

test('canceled diagnostic cannot overwrite a new result or re-enable its buttons',async()=>{
 const f=fixture();f.$('browser-mode').value='cdp';
 const old=f.ctx.testBrowserEnvironment(true);
 assert.equal(f.calls[0].options.timeoutMS,50000);
 f.ctx.cancelBrowserDiagnostic();assert.equal(f.calls[0].options.signal.aborted,true);
 assert.equal(f.$('test-chrome').disabled,false);
 f.$('browser-mode').value='local';
 const current=f.ctx.testBrowserEnvironment(false);
 f.calls[0].resolve(response);await old;
 assert.equal(f.$('test-chrome').disabled,true);assert.equal(f.$('browser-connection-result').textContent,'');
 f.calls[1].resolve(response);await current;
 assert.match(f.$('chrome-result').textContent,/本地 Chrome/);
 assert.equal(f.$('test-chrome').disabled,false);assert.deepEqual(f.toasts,[]);
});

test('timeout releases diagnostic state and retry uses the current endpoint',async()=>{
 const f=fixture();f.$('browser-mode').value='cdp';
 const first=f.ctx.testBrowserEnvironment(true);
 f.calls[0].reject(Object.assign(new Error('请求超时'),{name:'TimeoutError'}));await first;
 assert.match(f.$('browser-connection-result').textContent,/超时/);
 assert.equal(f.$('test-browser-connection').disabled,false);assert.equal(f.$('test-chrome').disabled,false);
 f.$('cdp-port').value='9222';const retry=f.ctx.testBrowserEnvironment(true);
 assert.equal(f.calls[1].data.cdp_port,9222);f.calls[1].resolve(response);await retry;
 assert.match(f.$('browser-connection-result').textContent,/127.0.0.1:9222/);
});

test('aborted diagnostics remain silent and repeated clicks do not start another request',async()=>{
 const f=fixture();const pending=f.ctx.testBrowserEnvironment(true);
 await f.ctx.testBrowserEnvironment(false);assert.equal(f.calls.length,1);
 f.ctx.cancelBrowserDiagnostic();f.calls[0].reject(Object.assign(new Error('canceled'),{name:'AbortError'}));await pending;
 assert.deepEqual(f.toasts,[]);assert.equal(f.$('browser-connection-result').textContent,'');
});
