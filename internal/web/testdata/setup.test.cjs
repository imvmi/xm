const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/setup.js'),'utf8');

async function fixture(initialized,driver='mysql'){
 const nodes=new Map(),requests=[];
 const node=id=>{
  if(!nodes.has(id))nodes.set(id,{id,value:'',textContent:'',hidden:false,disabled:false,checked:false,dataset:{},attrs:{},listeners:{},willValidate:true,selectedOptions:[{textContent:'本地文件系统'}],
   classList:{toggle(){}},focus(){},setAttribute(k,v){this.attrs[k]=v},removeAttribute(k){delete this.attrs[k]},setCustomValidity(v){this.invalid=v},reportValidity(){return !this.invalid},
   addEventListener(k,fn){this.listeners[k]=fn},querySelector(s){return node(id+'/'+s)},querySelectorAll(){return (sectionFields[id]||[]).map(node)}});
  return nodes.get(id);
 };
 const sectionFields={
  'step-database':['db-host','db-port','db-name','db-user'],
  'step-storage':['media-kind'],
  'step-account':['setup-username','setup-password','setup-confirm'],
  'step-existing':['existing-username','existing-password']
 };
 const fields=['db-host','db-port','db-name','db-user','db-password','db-password-env','db-tls','db-ca','db-server-name','media-kind','media-name','media-endpoint','media-prefix','media-user','media-password','media-bucket','media-region','media-access','media-secret','media-session','media-path-style','media-timeout','setup-username','setup-password','setup-confirm','existing-username','existing-password'].map(node);
 const radios=['sqlite','mysql','mariadb'].map(driver=>{const r=node(driver+'-radio');r.value=driver;return r;});radios.find(r=>r.value===driver).checked=true;
 const steps=[0,1,2].map(i=>node('progress-'+i));
 const state={initialized,connected:false,connectError:false};
 const document={getElementById:node,
  querySelector(selector){if(selector.endsWith(':checked'))return radios.find(r=>r.checked);return radios.find(r=>selector.includes('value="'+r.value+'"'))},
  querySelectorAll(selector){if(selector==='[data-step]')return steps;if(selector==='input[name="db-kind"]')return radios;if(selector==='input[type="password"]')return fields.filter(n=>/password|confirm|secret|token/.test(n.id));return fields}
 };
 const fetch=async(url,options)=>{
  const route=url.split('/').at(-1);
  const payload=options?.body?JSON.parse(options.body):undefined;
  requests.push({route,payload});
  let status=200,data={};
  if(route==='status')data={needed:!state.connected};
  if(route==='defaults')data={database:{driver,host:'db.example.test',port:3306,name:'archive',user:'xm',tls_mode:'verify_identity'}};
  if(route==='database-test')data={initialized:state.initialized};
  if(route==='connect'&&state.connectError){status=401;data={error:'原管理员用户名或密码不正确'}}
  else if(route==='connect'||route==='complete'){state.connected=true;data={ok:true}}
  return {ok:status===200,json:async()=>data};
 };
 node('media-kind').value='local';node('media-timeout').value='120';
 node('setup-username').value='new-admin';node('setup-password').value='new-password';node('setup-confirm').value='new-password';
 node('existing-username').value='original-admin';node('existing-password').value='original-password';
 const context=vm.createContext({document,fetch,setTimeout,location:{replace(){}}});
 vm.runInContext(source,context);
 await new Promise(setImmediate);
 return {node,requests,state,steps,submit:()=>node('setup-form').listeners.submit({preventDefault(){}}),back:()=>node('setup-back').listeners.click()};
}

test('existing database skips storage and submits only original credentials and tested destination',async()=>{
 const f=await fixture(true);
 f.node('setup-confirm').value='mismatched-new-password';
 await f.submit();
 assert.equal(f.node('step-existing').hidden,false);
 assert.equal(f.node('step-storage').hidden,true);
 assert.equal(f.node('step-account').hidden,true);
 assert.equal(f.node('step-count').textContent,'02 / 02');
 assert.equal(f.steps[2].hidden,true);
 // Hidden/stale form values must not redirect the verified connection or set storage.
 f.node('db-name').value='different-database';f.node('media-kind').value='s3';
 await f.submit();
 const sent=f.requests.find(r=>r.route==='connect').payload;
 assert.equal(sent.username,'original-admin');assert.equal(sent.password,'original-password');
 assert.equal(sent.database.name,'archive');assert.equal('storage' in sent,false);assert.equal('storage_kind' in sent,false);
 assert.equal(f.requests.some(r=>['storage-test','complete'].includes(r.route)),false);
 assert.match(f.node('setup-success-description').textContent,/原管理员/);
 assert.equal(f.node('existing-password').value,'');assert.equal(f.node('setup-login').disabled,false);
});

test('wrong existing credentials stay on verification step and can be retried',async()=>{
 const f=await fixture(true);await f.submit();f.state.connectError=true;
 await f.submit();
 assert.match(f.node('setup-error').textContent,/密码不正确/);
 assert.equal(f.node('step-existing').hidden,false);assert.equal(f.node('setup-form').hidden,false);
 assert.equal(f.node('setup-next').disabled,false);
 f.state.connectError=false;await f.submit();assert.equal(f.state.connected,true);
});

test('returning to database and selecting a new database restores the three-step setup',async()=>{
 const f=await fixture(true);await f.submit();f.back();
 f.state.initialized=false;await f.submit();
 assert.equal(f.node('step-storage').hidden,false);assert.equal(f.node('step-existing').hidden,true);
 assert.equal(f.node('step-count').textContent,'02 / 03');assert.equal(f.steps[2].hidden,false);
 await f.submit();assert.equal(f.node('step-account').hidden,false);
 await f.submit();
 assert.equal(f.requests.some(r=>r.route==='connect'),false);
 assert.equal(f.requests.filter(r=>r.route==='storage-test').length,1);
 assert.equal(f.requests.find(r=>r.route==='complete').payload.username,'new-admin');
});

for(const initialized of [false,true])test(`MariaDB defaults and ${initialized?'existing connection':'new setup'} preserve the backend`,async()=>{
 const f=await fixture(initialized,'mariadb');
 assert.equal(f.node('mariadb-radio').checked,true);
 assert.equal(f.node('mysql-fields').hidden,false);
 assert.equal(f.node('mysql-fields').disabled,false);
 assert.equal(f.node('sqlite-note').hidden,true);
 await f.submit();
 if(!initialized){
  await f.submit();
  assert.match(f.node('setup-summary').textContent,/MariaDB \/ archive/);
  f.node('setup-username').listeners.input();
  assert.match(f.node('setup-summary').textContent,/MariaDB \/ archive/);
 }
 await f.submit();
 const writes=f.requests.filter(r=>['database-test','storage-test','complete','connect'].includes(r.route));
 assert.ok(writes.length>=2);
 for(const r of writes)assert.equal(r.payload.database.driver,'mariadb');
 assert.equal(f.state.connected,true);
});
