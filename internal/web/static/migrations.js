'use strict';
let migrationState=null, migrationChecked=null, migrationRetry=false, migrationRefreshing=false, migrationPreviewVersion=0, migrationConfigKey='';
const migrationLabels={running:'执行中',switching:'正在重启',paused:'已暂停',failed:'需重试',completed:'已完成'};
const migrationDatabaseName=value=>value==='mysql'?'MySQL':'SQLite3';
const migrationStorageName=id=>id==='local'?'本地磁盘':id==='new'?'新的远端存储':migrationState?.sources?.find(p=>p.id===id)?.name||id;
function resetMigrationPreview(){
 migrationPreviewVersion++;migrationChecked=null;migrationRetry=false;
 $('migration-preview-panel').hidden=true;$('migration-confirm-panel').hidden=true;$('migration-confirm').value='';$('migration-start').disabled=true;$('migration-error').textContent='';
}
function migrationStorageTargets(){
 const source=$('migration-source').value,previous=$('migration-storage-target').value,active=migrationState?.active_storage;
 const targets=source==='local'?(migrationState?.sources||[]).filter(p=>active==='local'||p.id===active).map(p=>({id:p.id,name:p.name})).concat(active==='local'?[{id:'new',name:'添加远端存储…'}]:[]):[{id:'local',name:'本地磁盘'}];
 $('migration-storage-target').innerHTML=targets.map(p=>`<option value="${escape(p.id)}">${escape(p.name)}</option>`).join('');
 if(targets.some(p=>p.id===previous))$('migration-storage-target').value=previous;
 migrationAvailability();
}
function migrationAvailability(){
 const busy=['running','switching'].includes(migrationState?.job?.status),mysql=migrationState?.database_driver==='mysql';
 $('migration-database-current').textContent='当前 '+migrationDatabaseName(migrationState?.database_driver);
 $('migration-database-source').textContent=migrationDatabaseName(migrationState?.database_driver);
 $('migration-database-target').textContent=mysql?'SQLite3 · 服务器本地':'MySQL';
 $('migration-mysql-fields').hidden=mysql;
 $('migration-database-help').textContent=mysql?'校验完成后切换到本地 SQLite3，并自动重启应用。':'填写独立的空 MySQL 数据库，校验完成后切换并自动重启应用。';
 $('migration-storage-current').textContent='默认 '+migrationStorageName(migrationState?.active_storage);
 $('migration-storage-help').textContent=$('migration-source').value==='local'?'将本地媒体上传到远端，回读校验后启用远端引用。':'将所选远端媒体下载到服务器本地，逐文件校验后切换引用。';
 $('migration-remote-fields').hidden=$('migration-storage-target').value!=='new';
 $('migration-s3-fields').hidden=$('migration-remote-kind').value!=='s3';
 $('migration-webdav-fields').hidden=$('migration-remote-kind').value!=='webdav';
 document.querySelectorAll('.migration-sections input,.migration-sections select,.migration-sections button').forEach(el=>{el.disabled=busy;});
 $('migration-preview').disabled=busy||!migrationState||!$('migration-storage-target').value;
 $('migration-database-preview').disabled=busy||!migrationState;
 if(busy){$('migration-confirm-panel').hidden=true;$('migration-start').disabled=true;}
}
async function refreshMigrations(){
 const data=await api('/migrations');migrationState=data;
 const key=JSON.stringify([data.database_driver,data.active_storage,data.sources]);
 if(key!==migrationConfigKey){
  resetMigrationPreview();migrationConfigKey=key;
  const previous=$('migration-source').value;
  const sources=[{id:'local',name:'本地磁盘'},...(data.sources||[]).filter(p=>data.active_storage==='local'||p.id===data.active_storage)];
  $('migration-source').innerHTML=sources.map(p=>`<option value="${escape(p.id)}">${escape(p.name)}</option>`).join('');
  $('migration-source').value=sources.some(p=>p.id===previous)?previous:data.active_storage;
  migrationStorageTargets();
 }
 migrationAvailability();
 const j=data.job;$('migration-job-panel').hidden=!j;$('migration-badge').textContent=j?(migrationLabels[j.status]||j.status):'未开始';
 if(!j)return;
 const source=j.kind==='database'?migrationDatabaseName(j.source):migrationStorageName(j.source),target=j.kind==='database'?migrationDatabaseName(j.target||'sqlite'):migrationStorageName(j.target||'local');
 $('migration-job-kind').textContent=(j.kind==='database'?'数据库':'存储')+' · '+source+' → '+target+' · '+date(j.updated_at);
 $('migration-phase').textContent=j.phase+(data.maintenance?' · 维护模式：等待当前任务结束并暂停新写入':'');
 const p=$('migration-progress');p.max=Math.max(j.total,1);p.value=Math.min(j.done+j.failed,p.max);if(j.status==='completed')p.value=p.max;
 p.dataset.status=j.status;if(j.status==='running'&&!j.total)p.removeAttribute('value');
 $('migration-counts').textContent=`已完成 ${number(j.done)} / ${number(j.total)}${j.kind==='database'?' 行次（复制 + 校验）':` 个文件 · ${bytes(j.bytes)} / ${bytes(j.total_bytes)} · 失败 ${number(j.failed)}`}`;
 $('migration-message').textContent=j.message||'';
 $('migration-pause').hidden=j.status!=='running';$('migration-retry').hidden=!['paused','failed'].includes(j.status);
 $('migration-failures-panel').hidden=!j.failures?.length;
 $('migration-failures').innerHTML=(j.failures||[]).map(f=>`<li>推文 ${escape(f.post_id)} / 媒体 ${escape(f.media_id)}：${escape(f.message)}</li>`).join('');
}
function migrationInput(kind){
 if(kind==='database'){
  const target=migrationState.database_driver==='mysql'?'sqlite':'mysql';
  const input={kind,source:migrationState.database_driver,target};
  if(target==='mysql')input.database={driver:'mysql',host:$('migration-mysql-host').value.trim(),port:Number($('migration-mysql-port').value),name:$('migration-mysql-name').value.trim(),user:$('migration-mysql-user').value.trim(),password:$('migration-mysql-password').value,password_env:$('migration-mysql-password-env').value.trim(),tls_mode:$('migration-mysql-tls').value,ca_file:$('migration-mysql-ca').value.trim(),server_name:$('migration-mysql-server-name').value.trim()};
  return input;
 }
 const input={kind,source:$('migration-source').value,target:$('migration-storage-target').value};
 if(input.target==='new'){
  const storage={kind:$('migration-remote-kind').value,name:$('migration-remote-name').value.trim(),endpoint:$('migration-remote-endpoint').value.trim(),prefix:$('migration-remote-prefix').value.trim(),timeout_seconds:Number($('migration-remote-timeout').value),use_proxy:$('migration-remote-proxy').checked};
  if(storage.kind==='s3')Object.assign(storage,{bucket:$('migration-s3-bucket').value.trim(),region:$('migration-s3-region').value.trim(),access_key:$('migration-s3-access-key').value,secret_key:$('migration-s3-secret-key').value,session_token:$('migration-s3-token').value,path_style:$('migration-s3-path-style').checked});
  else Object.assign(storage,{username:$('migration-webdav-user').value,password:$('migration-webdav-password').value});
  input.storage=storage;
 }
 return input;
}
async function checkMigration(kind,retry=false){
 resetMigrationPreview();migrationRetry=retry;
 const version=migrationPreviewVersion;
 try{
  const input=migrationInput(kind);
  const p=await api('/migrations/preview','POST',input);if(version!==migrationPreviewVersion)return;
  migrationChecked=input;
  $('migration-preview-title').textContent=(kind==='database'?'数据库':'存储')+'迁移检查通过';
  $('migration-preview-summary').textContent=`${kind==='database'?'推文':'待迁移文件'} ${number(p.items)} · 估计数据 ${bytes(p.bytes)} · 本地所需空间 ${bytes(p.required_bytes)} · 本地可用 ${bytes(p.free_bytes)} · 目标：${p.destination}`;
  $('migration-preview-warning').textContent=p.warning;$('migration-preview-panel').hidden=false;$('migration-confirm-panel').hidden=false;
  $('migration-start').textContent=retry?'确认继续 / 重试':'确认开始迁移';$('migration-confirm').focus();
 }catch(e){if(version===migrationPreviewVersion)$('migration-error').textContent=e.message;}
}
document.querySelectorAll('.migration-sections input,.migration-sections select').forEach(el=>el.addEventListener('input',resetMigrationPreview));
$('migration-source').addEventListener('change',()=>{resetMigrationPreview();migrationStorageTargets();});
$('migration-storage-target').addEventListener('change',()=>{resetMigrationPreview();migrationAvailability();});
$('migration-remote-kind').addEventListener('change',()=>{resetMigrationPreview();migrationAvailability();});
$('migration-confirm').addEventListener('input',()=>{$('migration-start').disabled=!migrationChecked||$('migration-confirm').value!=='开始迁移'||['running','switching'].includes(migrationState?.job?.status);});
$('migration-database-preview').addEventListener('click',()=>action($('migration-database-preview'),()=>checkMigration('database')));
$('migration-preview').addEventListener('click',()=>action($('migration-preview'),()=>checkMigration('media')));
$('migration-retry').addEventListener('click',()=>action($('migration-retry'),async()=>{
 const j=migrationState?.job;if(!j)return;
 if(j.kind==='media'){
  $('migration-source').value=j.source;migrationStorageTargets();
  const known=j.target==='local'||migrationState.sources.some(p=>p.id===j.target);
  $('migration-storage-target').value=known?j.target:'new';migrationAvailability();
  if(!known){resetMigrationPreview();$('migration-error').textContent='该任务尚未保存目标连接，请填写远端配置后重新检查。';$('migration-remote-name').focus();return;}
 }
 if(j.kind==='database'&&j.target==='mysql'&&!$('migration-mysql-host').value){resetMigrationPreview();$('migration-error').textContent='请重新填写空 MySQL 目标连接后检查。密码不会保存在迁移记录中。';$('migration-mysql-host').focus();return;}
 await checkMigration(j.kind,true);
}));
$('migration-start').addEventListener('click',()=>action($('migration-start'),async()=>{
 if(!migrationChecked||$('migration-confirm').value!=='开始迁移')return;
 await api('/migrations','POST',{...migrationChecked,retry:migrationRetry,confirm:$('migration-confirm').value});
 document.querySelectorAll('.migration-sections input[type="password"]').forEach(el=>{el.value='';});
 resetMigrationPreview();await refreshMigrations();toast('迁移任务已启动，源数据会保留');
}));
$('migration-pause').addEventListener('click',()=>action($('migration-pause'),async()=>{const r=await api('/migrations/pause','POST');toast(r.message);await refreshMigrations();}));
setInterval(async()=>{
 if(!state.authenticated||state.view!=='settings'||document.hidden||migrationRefreshing)return;
 migrationRefreshing=true;
 try{await refreshMigrations();}catch(e){$('migration-error').textContent=migrationState?.job?.kind==='database'?'数据库切换可能正在重启；请稍候刷新并重新登录。':e.message;}finally{migrationRefreshing=false;}
},2000);
