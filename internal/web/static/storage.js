"use strict";
async function refreshStorage(){
 const storage=await api('/storage');
 const p=storage.profiles.find(p=>p.id===storage.active);
 $('storage-badge').textContent=p?.name||'本地磁盘';
 $('storage-readonly-name').textContent=p?(p.kind==='s3'?'S3 · ':'WebDAV · ')+p.name:'本地文件系统';
 $('storage-readonly-detail').textContent=p?[p.endpoint||'AWS S3',p.bucket,p.region,p.prefix].filter(Boolean).join(' · '):'媒体保存在数据目录下的 media 文件夹中。';
}
