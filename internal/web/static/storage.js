"use strict";
async function refreshStorage(){
 const storage=await api('/storage');
 renderStorageCredentialProfiles(storage.profiles);
 const p=storage.profiles.find(p=>p.id===storage.active);
 $('storage-badge').textContent=p?.name||'本地磁盘';
 $('storage-readonly-name').textContent=p?({s3:'S3 · ',webdav:'WebDAV · ',smb:'Samba · '}[p.kind]||'')+p.name:'本地文件系统';
 $('storage-readonly-detail').textContent=p?[p.endpoint||'AWS S3',p.bucket,p.region,p.prefix].filter(Boolean).join(' · '):'媒体保存在数据目录下的 media 文件夹中。';
 const cache=storage.cache||{enabled:false,max_bytes:2*1024**3,used_bytes:0};
 const remote=['webdav','s3','smb'].includes(p?.kind);
 $('remote-cache-panel').hidden=!remote;
 $('remote-cache-note').hidden=!remote;
 hydrateSettingsForm('remote-cache-form',()=>{$('remote-cache-enabled').checked=!!cache.enabled;
 $('remote-cache-size').value=Math.round(cache.max_bytes/1024**2);});
 $('remote-cache-usage').textContent=bytes(cache.used_bytes)+' / '+bytes(cache.max_bytes);
 $('remote-cache-clear').disabled=!cache.used_bytes;
 $('remote-cache-badge').textContent=cache.enabled?'已启用':'未启用';
 $('remote-cache-badge').className='badge '+(cache.enabled?'':'neutral');
 await refreshAvatarCache();
}
$('remote-cache-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
 const submitted=settingsSnapshot('remote-cache-form');
 const enabled=$('remote-cache-enabled').checked,maxBytes=Math.round(Number($('remote-cache-size').value)*1024**2);
 await api('/storage/cache','PUT',{enabled,max_bytes:maxBytes});acceptSettingsForm('remote-cache-form',submitted);
 $('remote-cache-result').textContent=enabled?'服务器缓存已启用，首次访问远程媒体时开始填充。':'服务器缓存已停用，现有缓存文件会保留，仍可手动清空。';
 toast('服务器媒体缓存设置已保存');await refreshStorage();
});});
$('remote-cache-clear').addEventListener('click',()=>action($('remote-cache-clear'),async()=>{
 await api('/storage/cache','DELETE');$('remote-cache-result').textContent='服务器缓存已清空；归档源文件未受影响。';toast('服务器缓存已清空');await refreshStorage();
}));

async function refreshAvatarCache(){
 const info=await api('/settings/avatars');
 $('avatar-cache-status').textContent=`已保存 ${info.sources||0} 个来源 · 已缓存 ${info.cached||0} 张（${bytes(info.used_bytes||0)}）`;
}
$('avatar-cache-clear').addEventListener('click',()=>action($('avatar-cache-clear'),async()=>{
 const result=await api('/settings/avatars/cache','DELETE');
 $('avatar-cache-result').textContent=`已清理 ${result.cleared} 张头像缓存，下次查看时自动下载；原始 URL、推文和媒体已保留。`;
 await refreshAvatarCache();
}));

let storageCredentialProfiles=[];
function resetStorageCredentialSecrets(){
 for(const id of ['password','access','secret','token'])$('storage-credentials-'+id).value='';
 $('storage-credentials-clear-token').checked=false;
}
function selectStorageCredentialProfile(){
 resetStorageCredentialSecrets();
 const p=storageCredentialProfiles.find(p=>p.id===$('storage-credentials-profile').value);
 $('storage-credentials-login').hidden=!p||p.kind==='s3';
 $('storage-credentials-s3').hidden=!p||p.kind!=='s3';
 $('storage-credentials-username').value=p?.username||'';
 $('storage-credentials-destination').textContent=p?[p.endpoint||'AWS S3',p.bucket,p.region,p.prefix].filter(Boolean).join(' · '):'';
 $('storage-credentials-result').textContent='';
}
function renderStorageCredentialProfiles(profiles){
 storageCredentialProfiles=profiles;
 const select=$('storage-credentials-profile'),previous=select.value;
 select.innerHTML=profiles.map(p=>`<option value="${escape(p.id)}">${escape(p.name)}</option>`).join('');
 select.value=profiles.some(p=>p.id===previous)?previous:(profiles[0]?.id||'');
 $('storage-credentials-panel').hidden=!profiles.length;
 if(select.value!==previous)selectStorageCredentialProfile();
}
$('storage-credentials-profile').addEventListener('change',selectStorageCredentialProfile);
$('storage-credentials-clear-token').addEventListener('change',()=>{
 if($('storage-credentials-clear-token').checked)$('storage-credentials-token').value='';
});
$('storage-credentials-form').addEventListener('submit',event=>{
 event.preventDefault();action(event.submitter,async()=>{
  const id=$('storage-credentials-profile').value,p=storageCredentialProfiles.find(p=>p.id===id);
  if(!p)return;
  const input={};
  if(p.kind==='s3'){
   const access=$('storage-credentials-access').value,secret=$('storage-credentials-secret').value;
   if(access||secret){if(!access||!secret)throw new Error('请同时填写 Access Key 和 Secret Key');input.access_key=access;input.secret_key=secret;}
   if($('storage-credentials-clear-token').checked)input.session_token='';
   else if($('storage-credentials-token').value)input.session_token=$('storage-credentials-token').value;
  }else{
   if($('storage-credentials-username').value!==p.username)input.username=$('storage-credentials-username').value;
   if($('storage-credentials-password').value)input.password=$('storage-credentials-password').value;
  }
  if(!Object.keys(input).length)throw new Error('请填写需要更新的凭据');
  $('storage-credentials-result').textContent='正在验证存储凭据…';
  try{
   const result=await api('/storage/profiles/'+encodeURIComponent(id)+'/credentials','PUT',input);
   resetStorageCredentialSecrets();
   if(state.authenticated){$('storage-credentials-result').textContent=result.message;toast('存储凭据已更新');await refreshStorage();}
  }catch(err){resetStorageCredentialSecrets();$('storage-credentials-result').textContent=err.message;throw err;}
 });
});
