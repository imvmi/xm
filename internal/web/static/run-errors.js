'use strict';
const runErrorsState={id:0,offset:0,total:0,request:0,controller:null,busy:false};
function mediaProblemHTML(item){
 const ignored=item.status==='ignored';
 const labels={ignored:'已忽略 · 未下载',permanent:'永久失败 · 不会自动重试',failed:'下载失败',pending:'等待下次采集下载'};
 const waiting=item.status==='failed'&&Number(item.next_attempt)>Date.now()/1000;
 const retry=waiting?`最早重试：${date(item.next_attempt)}（本地时间，到期后随采集任务重试）`:item.status==='failed'?'已可重试，等待后续采集任务':'';
 const reason=item.error||'尚未记录具体错误';
 const hint=/HTTP\s+403\b/.test(reason)?'<p class="field-help">源站拒绝访问。地址失效、访问限制或媒体被禁用均可能导致 403，不能仅凭状态码确定原因。请查看原推文；若确认媒体已被删除或禁用，可手动忽略。</p>':'';
 const attrs=`data-media-post="${escape(item.post_id)}" data-media-id="${escape(item.id)}"`;
 const actions=ignored?`<button type="button" class="button secondary" data-media-operation="retry" ${attrs}>恢复重试</button>`:`${item.status==='pending'?'':`<button type="button" class="button secondary" data-media-operation="retry" ${attrs}>清除等待并恢复重试</button>`}<button type="button" class="button secondary" data-media-operation="ignore" ${attrs}>忽略此媒体</button>`;
 return `<article class="media-issue"><h4>${item.kind==='photo'?'图片':'视频'} · 推文 ${escape(item.post_id)}</h4><p class="media-issue-status">${escape(labels[item.status]||item.status)}${retry?' · '+escape(retry):''}</p><p class="run-error-message">${escape(reason)}</p>${hint}<p class="muted">尝试次数：${escape(String(item.attempts))} · 媒体 ID：${escape(item.id)}</p><div class="media-issue-links"><a href="#post/${encodeURIComponent(item.post_id)}" data-error-post-link>查看归档推文</a><a href="https://x.com/i/status/${encodeURIComponent(item.post_id)}" target="_blank" rel="noopener noreferrer">查看原推文 ↗</a></div><div class="media-issue-actions">${actions}</div></article>`;
}
async function loadRunErrors(append=false){
 const id=runErrorsState.id,request=++runErrorsState.request;
 runErrorsState.controller?.abort();
 const controller=new AbortController();runErrorsState.controller=controller;
 const offset=append?runErrorsState.offset:0;
 $('run-errors-status').textContent='正在读取详情…';
 $('run-errors-more').disabled=true;
 try{
  const data=await api(`/runs/${id}/errors?offset=${offset}`,'GET',null,{signal:controller.signal});
  if(request!==runErrorsState.request||!$('run-errors-dialog').open)return;
  $('run-errors-history').hidden=false;$('run-errors-current').hidden=false;
  $('run-errors-meta').textContent=`@${data.run.target} · ${date(data.run.started_at)} · ${runLabels[data.run.status]?.[0]||data.run.status}`;
  $('run-errors-message').textContent=stopLabels[data.run.message]||data.run.message||'该次任务未记录错误信息。';
  const html=data.items.map(mediaProblemHTML).join('');
  if(append)$('run-errors-media').insertAdjacentHTML('beforeend',html);
  else $('run-errors-media').innerHTML=html||'<p class="field-help">当前没有未下载或已忽略的媒体。历史任务记录仍保留当时的结果。</p>';
  runErrorsState.offset=offset+data.items.length;runErrorsState.total=data.total;
  $('run-errors-more').hidden=runErrorsState.offset>=data.total||!data.items.length;
  $('run-errors-status').textContent=data.total?`已显示 ${runErrorsState.offset} / ${data.total} 个未下载媒体（含已忽略）。`:'';
  $('run-errors-media').querySelectorAll('button').forEach(b=>b.disabled=runErrorsState.busy);
 }catch(err){
  if(request===runErrorsState.request&&$('run-errors-dialog').open&&!isRequestAbort(err))$('run-errors-status').textContent=err.message;
 }finally{
  if(request===runErrorsState.request)$('run-errors-more').disabled=false;
 }
}
function openRunErrors(id){
 if(!Number.isSafeInteger(id)||id<=0)return;
 runErrorsState.id=id;runErrorsState.offset=0;runErrorsState.total=0;
 $('run-errors-history').hidden=true;$('run-errors-current').hidden=true;
 $('run-errors-media').replaceChildren();$('run-errors-more').hidden=true;
 if(!$('run-errors-dialog').open)$('run-errors-dialog').showModal();
 loadRunErrors();
}
async function manageMediaProblem(button){
 if(runErrorsState.busy)return;
 const operation=button.dataset.mediaOperation;
 if(operation==='ignore'&&!window.confirm('确认忽略此媒体？它将停止自动重试，不再阻止后续任务完成，但仍显示为未下载。推文和错误记录保留，同一媒体在其他账号中的引用也会生效。之后可在此恢复重试。'))return;
 const id=runErrorsState.id;
 runErrorsState.busy=true;
 $('run-errors-media').querySelectorAll('button').forEach(b=>b.disabled=true);
 try{
  const result=await api(`/runs/${id}/media/${encodeURIComponent(button.dataset.mediaPost)}/${encodeURIComponent(button.dataset.mediaId)}/${operation}`,'POST');
  if($('run-errors-dialog').open&&runErrorsState.id===id){await loadRunErrors();$('run-errors-status').textContent=result.message;}
  toast(result.message);
  if(state.view==='activity')await refreshRuns();
  await refreshOverview();
 }catch(err){
  if($('run-errors-dialog').open&&runErrorsState.id===id)$('run-errors-status').textContent=err.message;
 }finally{
  runErrorsState.busy=false;
  $('run-errors-media').querySelectorAll('button').forEach(b=>b.disabled=false);
 }
}
$('runs').addEventListener('click',event=>{const button=event.target.closest('[data-run-errors]');if(button)openRunErrors(Number(button.dataset.runErrors));});
$('run-errors-media').addEventListener('click',event=>{
 const button=event.target.closest('[data-media-operation]');
 if(button)manageMediaProblem(button);
 if(event.target.closest('[data-error-post-link]'))$('run-errors-dialog').close();
});
$('run-errors-refresh').addEventListener('click',()=>loadRunErrors());
$('run-errors-more').addEventListener('click',()=>loadRunErrors(true));
$('run-errors-dialog').addEventListener('close',()=>{runErrorsState.request++;runErrorsState.controller?.abort();});
