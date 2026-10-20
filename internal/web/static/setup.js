'use strict';
const el=id=>document.getElementById(id);
const sections=['step-database','step-storage','step-account','step-existing'];
let step=0,busy=false,loaded=false,existing=false,testedDatabase=null;
const activeSections=()=>existing?[sections[0],sections[3]]:sections.slice(0,3);
el('setup-form').noValidate=true;
function showStep(next){
 const active=activeSections();step=next;sections.forEach(id=>el(id).hidden=id!==active[step]);
 document.querySelectorAll('[data-step]').forEach((item,i)=>{item.hidden=i>=active.length;if(i===step)item.setAttribute('aria-current','step');else item.removeAttribute('aria-current');item.classList.toggle('done',i<step);if(i===1){item.querySelector('strong').textContent=existing?'验证原账号':'媒体存储';item.querySelector('small').textContent=existing?'保留全部原有配置':'安放图片和视频';}});
 el('setup-back').hidden=step===0;el('step-count').textContent=`0${step+1} / 0${active.length}`;
 el('step-database').querySelector('.eyebrow').textContent=`STEP 01 / 0${active.length}`;
 el('setup-next').textContent=(existing?['测试数据库并继续 →','验证并接入 →']:['测试数据库并继续 →','测试存储并继续 →','完成设置 →'])[step];
 el('setup-error').textContent='';el('setup-status').textContent='';
 if(step===2){const db=document.querySelector('input[name="db-kind"]:checked').value;el('setup-summary').textContent=`管理员：${el('setup-username').value||'待填写'} · 数据库：${db==='sqlite'?'SQLite':'MySQL / '+el('db-name').value} · 存储：${el('media-kind').selectedOptions[0].textContent}`;}
 el(active[step]).querySelector('h2').focus({preventScroll:true});
}
function databaseChanged(){const mysql=document.querySelector('input[name="db-kind"]:checked').value==='mysql';el('mysql-fields').hidden=!mysql;el('mysql-fields').disabled=!mysql;el('sqlite-note').hidden=mysql;}
function storageChanged(){
 const kind=el('media-kind').value,remote=['webdav','s3','smb'].includes(kind),s3=kind==='s3',smb=kind==='smb';
 el('remote-fields').hidden=!remote;el('remote-fields').disabled=!remote;el('local-note').hidden=kind!=='local';
 el('webdav-fields').hidden=s3;el('webdav-fields').disabled=s3;el('s3-fields').hidden=!s3;el('s3-fields').disabled=!s3;
 el('media-endpoint').required=kind==='webdav'||smb;
 el('media-user').required=smb;el('media-password').required=smb;el('media-user-label').textContent=smb?'用户名（域账号：域名\\用户名）':'用户名';
 el('media-endpoint').placeholder=smb?'smb://192.168.1.10/共享名':s3?'https://s3.example.com（AWS S3 可留空）':'https://nas.example.com/remote.php/dav/files/user';
 el('media-endpoint-help').textContent=smb?'使用 SMB2/3 连接，默认端口 445；可写为 smb://服务器:端口/共享名，子目录填入目录前缀。':s3?'AWS S3 可以留空；兼容服务填写 Endpoint，Bucket 必须已存在。':'填写已存在的最终 WebDAV 根目录地址，不跟随重定向。';
 if(step===2)showStep(2);
}
function database(){
 const driver=document.querySelector('input[name="db-kind"]:checked').value;
 if(driver==='sqlite')return {driver};
 return {driver,host:el('db-host').value.trim(),port:Number(el('db-port').value),name:el('db-name').value.trim(),user:el('db-user').value,password:el('db-password').value,password_env:el('db-password-env').value.trim(),tls_mode:el('db-tls').value,ca_file:el('db-ca').value.trim(),server_name:el('db-server-name').value.trim()};
}
function storage(){return {name:el('media-name').value.trim(),endpoint:el('media-endpoint').value.trim().replace(/\/+$/,''),prefix:el('media-prefix').value.trim().replace(/^\/+|\/+$/g,''),username:el('media-user').value,password:el('media-password').value,bucket:el('media-bucket').value.trim(),region:el('media-region').value.trim(),access_key:el('media-access').value,secret_key:el('media-secret').value,session_token:el('media-session').value,path_style:el('media-path-style').checked,timeout_seconds:Number(el('media-timeout').value)};}
async function request(route,body){
 let r;try{r=await fetch('/api/setup/'+route,{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});}catch{throw new Error('无法连接服务，请确认服务仍在运行后重试。');}
 const data=await r.json().catch(()=>({error:'服务返回了无法识别的响应'}));if(!r.ok)throw new Error(data.error||'设置失败');return data;
}
function populate(c){
 const d=c.database||{};document.querySelector(`input[name="db-kind"][value="${d.driver==='mysql'?'mysql':'sqlite'}"]`).checked=true;
 const mapping={host:'host',port:'port',name:'name',user:'user',password_env:'password-env',tls_mode:'tls',ca_file:'ca',server_name:'server-name'};
 for(const [key,id] of Object.entries(mapping))if(d[key])el('db-'+id).value=d[key];
 if(c.password_configured){el('db-password').placeholder='留空保留已配置的密码';el('db-password-help').textContent='已有密码不会回显。同一数据库连接留空保留，修改目标后请重新填写。';}
 if(c.has_remote_storage){el('storage-keep').hidden=false;el('storage-keep').disabled=false;el('media-kind').value='keep';}
 databaseChanged();storageChanged();
}
function validStep(){
 el('setup-confirm').setCustomValidity(el('setup-password').value===el('setup-confirm').value?'':'两次密码不一致');
 for(const field of el(activeSections()[step]).querySelectorAll('input,select'))if(field.willValidate&&!field.reportValidity())return false;
 return true;
}
function setBusy(value){busy=value;el('setup-next').disabled=value;el('setup-back').disabled=value;el('setup-form').setAttribute('aria-busy',String(value));for(const field of document.querySelectorAll('#setup-form input,#setup-form select')){if(value){field.dataset.wasDisabled=String(field.disabled);field.disabled=true;}else if(field.dataset.wasDisabled!==undefined){field.disabled=field.dataset.wasDisabled==='true';delete field.dataset.wasDisabled;}}}
async function ready(){
 el('setup-form').hidden=true;el('setup-success').hidden=false;
 if(existing)el('setup-success-description').textContent='已接入原有数据库，管理员、存储及其他配置均已保留。请使用原管理员用户名和密码登录。';
 document.querySelectorAll('input[type="password"]').forEach(input=>input.value='');
 for(let i=0;i<25;i++){
  try{const r=await fetch('/api/setup/status',{cache:'no-store'});const s=await r.json();if(r.ok&&s.needed===false){el('setup-ready').textContent='服务已就绪，请登录。';el('setup-login').disabled=false;return;}}catch{}
  await new Promise(resolve=>setTimeout(resolve,600));
 }
 el('setup-ready').textContent='设置已保存，服务尚未就绪。请检查服务日志，稍后再打开登录页面。';el('setup-login').disabled=false;
}
el('setup-form').addEventListener('submit',async e=>{
 e.preventDefault();if(busy)return;if(!loaded){await loadDefaults();return;}if(!validStep())return;el('setup-error').textContent='';
 // Snapshot user input before disabling the controls during network requests.
 const payload=existing&&step===1?{username:el('existing-username').value,password:el('existing-password').value,database:testedDatabase}:{username:el('setup-username').value,password:el('setup-password').value,database:database(),storage_kind:el('media-kind').value,storage:storage()};
 setBusy(true);el('setup-status').textContent=existing&&step===1?'正在验证原管理员身份并保存本机连接配置…':['正在连接数据库并检查初始化状态…','正在测试媒体存储读写…','正在验证并保存账号与配置…'][step];
 try{
  if(step===0){const result=await request('database-test',{database:payload.database});existing=result.initialized===true;testedDatabase=payload.database;setBusy(false);showStep(1);}
  else if(existing){await request('connect',payload);await ready();}
  else if(step===1){await request('storage-test',{database:payload.database,storage_kind:payload.storage_kind,storage:payload.storage});setBusy(false);showStep(2);}
  else{await request('complete',payload);await ready();}
 }catch(err){el('setup-error').textContent=err.message;el('setup-status').textContent='';}
 finally{if(busy)setBusy(false);}
});
el('setup-back').addEventListener('click',()=>{if(!busy)showStep(step-1);});
document.querySelectorAll('input[name="db-kind"]').forEach(input=>input.addEventListener('change',databaseChanged));
el('media-kind').addEventListener('change',storageChanged);
el('setup-confirm').addEventListener('input',()=>el('setup-confirm').setCustomValidity(''));
el('setup-login').addEventListener('click',()=>location.replace('/'));
fetch('/api/setup/status',{cache:'no-store'}).then(r=>r.json()).then(s=>{if(!s.needed)location.replace('/');}).catch(()=>{});
databaseChanged();storageChanged();

async function loadDefaults(){
 setBusy(true);el('setup-status').textContent='正在读取数据库连接配置…';
 try{const cfg=await request('defaults',{});setBusy(false);populate(cfg);loaded=true;showStep(0);}
 catch(err){el('setup-error').textContent=err.message+' 点击继续以重试读取。';el('setup-status').textContent='';}
 finally{if(busy)setBusy(false);}
}
el('setup-username').addEventListener('input',()=>{if(step===2){const db=database();el('setup-summary').textContent=`管理员：${el('setup-username').value||'待填写'} · 数据库：${db.driver==='sqlite'?'SQLite':'MySQL / '+db.name} · 存储：${el('media-kind').selectedOptions[0].textContent}`;}});
loadDefaults();
