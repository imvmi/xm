"use strict";
async function refreshStorage(){
 const storage=await api('/storage');
 const p=storage.profiles.find(p=>p.id===storage.active);
 $('storage-badge').textContent=p?.name||'本地磁盘';
 $('storage-readonly-name').textContent=p?(p.kind==='s3'?'S3 · ':'WebDAV · ')+p.name:'本地文件系统';
 $('storage-readonly-detail').textContent=p?[p.endpoint||'AWS S3',p.bucket,p.region,p.prefix].filter(Boolean).join(' · '):'媒体保存在数据目录下的 media 文件夹中。';
 const cache=storage.cache||{enabled:false,max_bytes:2*1024**3,used_bytes:0};
 $('remote-cache-panel').hidden=!p;
 $('remote-cache-enabled').checked=!!cache.enabled;
 $('remote-cache-size').value=Math.round(cache.max_bytes/1024**2);
 $('remote-cache-usage').textContent=bytes(cache.used_bytes)+' / '+bytes(cache.max_bytes);
 $('remote-cache-clear').disabled=!cache.used_bytes;
 $('remote-cache-badge').textContent=cache.enabled?'已启用':'未启用';
 $('remote-cache-badge').className='badge '+(cache.enabled?'':'neutral');
}
$('remote-cache-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
 const enabled=$('remote-cache-enabled').checked,maxBytes=Math.round(Number($('remote-cache-size').value)*1024**2);
 await api('/storage/cache','PUT',{enabled,max_bytes:maxBytes});
 $('remote-cache-result').textContent=enabled?'本地缓存已启用，首次访问远程媒体时开始填充。':'本地缓存已停用，现有缓存文件会保留，仍可手动清空。';
 toast('远程媒体缓存设置已保存');await refreshStorage();
});});
$('remote-cache-clear').addEventListener('click',()=>action($('remote-cache-clear'),async()=>{
 await api('/storage/cache','DELETE');$('remote-cache-result').textContent='本地缓存已清空；归档源文件未受影响。';toast('本地缓存已清空');await refreshStorage();
}));
