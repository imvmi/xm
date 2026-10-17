'use strict';
let storageState={active:'local',profiles:[]};
const storageDestFields=['kind','endpoint','prefix','bucket','region','path-style'];
function storageKindChanged(){
  const s3=$('storage-kind').value==='s3';
  $('storage-webdav-fields').hidden=s3;$('storage-s3-fields').hidden=!s3;
  $('storage-endpoint').required=!s3;
  $('storage-bucket').required=s3;$('storage-region').required=s3;
  $('storage-access-key').required=s3&&!$('storage-edit').value;$('storage-secret-key').required=s3&&!$('storage-edit').value;
  $('storage-endpoint').placeholder=s3?'https://s3.example.com':'https://nas.example.com/remote.php/dav/files/user';
  $('storage-endpoint-help').textContent=s3?'自定义 S3 Endpoint；使用 AWS S3 时可留空。Bucket 需提前创建。':'填写已存在的 WebDAV 根目录 URL，使用最终地址，服务不跟随重定向。';
}
function editStorage(id=''){
  const p=storageState.profiles.find(p=>p.id===id);$('storage-edit').value=p?.id||'';
  $('storage-profile-form').reset();
  for(const key of ['name','kind','endpoint','prefix','username','bucket','region']){
    $("storage-"+key).value=p?.[key]??({kind:'webdav',region:'us-east-1'}[key]||'');
  }
  $('storage-timeout').value=p?.timeout_seconds||120;$('storage-path-style').checked=p?.path_style??true;$('storage-use-proxy').checked=!!p?.use_proxy;
  for(const key of storageDestFields)$("storage-"+key).disabled=!!p;
  $('storage-test').disabled=!p;$('storage-clear-token-label').hidden=!p?.token_configured;
  $('storage-secret-help').textContent=p?`凭据不会回显，留空保留。${p.password_configured?'已保存密码。':''}${p.keys_configured?'已保存 S3 密钥。':''}${p.token_configured?'已保存 Session Token。':''}`:'凭据保存在本机配置文件中，不会回显。';
  $('storage-destination-help').textContent=p?'已有连接的目标地址固定，以便继续读取旧文件。更换地址请新增连接。':'填写相对目录。WebDAV 会自动创建子目录。';
  $('storage-result').textContent='测试会写入、读取并删除一个随机测试文件。';storageKindChanged();
}
async function refreshStorage(selected){
  storageState=await api('/storage');
  const options=storageState.profiles.map(p=>`<option value="${escape(p.id)}">${escape(p.name)} · ${p.kind==='s3'?'S3':'WebDAV'}</option>`).join('');
  $('storage-active').innerHTML='<option value="local">本地磁盘</option>'+options;$('storage-active').value=storageState.active;
  $('storage-edit').innerHTML='<option value="">新增远程存储…</option>'+options;
  $('storage-badge').textContent=storageState.profiles.find(p=>p.id===storageState.active)?.name||'本地磁盘';
  editStorage(selected??(storageState.active==='local'?'':storageState.active));
}
$('storage-kind').addEventListener('change',storageKindChanged);
$('storage-edit').addEventListener('change',()=>editStorage($('storage-edit').value));
$('storage-new').addEventListener('click',()=>{editStorage();$('storage-name').focus();});
$('storage-active-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  await api('/storage/active','PUT',{id:$('storage-active').value});await refreshStorage();toast('存储已切换，下一个任务生效；已有文件保留原位置');
});});
$('storage-profile-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const p={id:$('storage-edit').value};
  for(const key of ['name','kind','endpoint','prefix','username','password','bucket','region'])p[key]=$('storage-'+key).value;
  for(const key of ['access-key','secret-key','session-token'])p[key.replaceAll('-','_')]=$('storage-'+key).value;
  p.path_style=$('storage-path-style').checked;p.use_proxy=$('storage-use-proxy').checked;p.clear_token=$('storage-clear-token').checked;p.timeout_seconds=Number($('storage-timeout').value);
  const result=await api('/storage/profiles','POST',p);await refreshStorage(result.id);toast('连接已保存，可先测试，再选择用于后续媒体');
});});
$('storage-test').addEventListener('click',()=>action($('storage-test'),async()=>{
  $('storage-result').textContent='正在测试已保存的连接…';
  try{const r=await api('/storage/profiles/'+encodeURIComponent($('storage-edit').value)+'/test','POST');$('storage-result').textContent=r.message;toast(r.message);}catch(err){$('storage-result').textContent=err.message;throw err;}
}));
