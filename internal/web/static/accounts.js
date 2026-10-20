'use strict';
let accountScroll=0,accountRestoring=false;
const accountPending=new Set(),accountExpanded=new Set();
const archiveOverview=$('archive-overview'),archiveOverviewToggle=$('archive-overview-toggle');
function positionArchiveOverview(){
 const anchor=archiveOverviewToggle.getBoundingClientRect(),width=Math.min(560,innerWidth-32);
 const top=Math.max(16,Math.min(anchor.bottom+8,innerHeight-240));
 archiveOverview.style.left=Math.max(16,Math.min(anchor.left,innerWidth-width-16))+'px';
 archiveOverview.style.top=top+'px';
 archiveOverview.style.maxHeight=Math.max(0,innerHeight-top-16)+'px';
}
archiveOverview.addEventListener('beforetoggle',event=>{
 if(event.newState==='open')positionArchiveOverview();
 archiveOverviewToggle.setAttribute('aria-expanded',String(event.newState==='open'));
});
window.addEventListener('resize',()=>{if(archiveOverview.matches(':popover-open'))positionArchiveOverview();});
window.addEventListener('scroll',()=>{if(archiveOverview.matches(':popover-open'))positionArchiveOverview();},{passive:true});
window.addEventListener('hashchange',()=>archiveOverview.hidePopover());
// Hash navigation may reset scroll before hashchange fires. Capture it before
// link navigation, and ignore scroll events caused by restoring the directory.
document.addEventListener('click',event=>{
 if(state.view==='archive'&&event.target.closest('a[href^="#"]'))accountScroll=window.scrollY;
},true);
window.addEventListener('scroll',()=>{
 if(state.view==='archive'&&location.hash==='#accounts'&&!accountRestoring)accountScroll=window.scrollY;
},{passive:true});
function accountRows(){
 const monitors=new Map(state.monitors.map(m=>[m.username,m]));
 const users=new Map(state.users.map(u=>[u.username,u]));
 if(monitorListReady)for(const m of state.monitors)if(!users.has(m.username))users.set(m.username,{username:m.username,name:handle(m),stats_pending:true});
 return [...users.values()].map(u=>{
  const m=monitorListReady?monitors.get(u.username):null;
  return {...u,...m,posts:u.posts,media:u.media,stats_pending:u.stats_pending,
   monitored:monitorListReady?!!m:!!u.monitored,runtime_pending:!monitorListReady&&u.monitored};
 });
}
function accountNeedsAttention(u){return !!(u.monitored&&(u.last_error||['incomplete','media_failed','failed','interrupted'].includes(u.last_status)||u.baseline_status==='blocked'));}
function filteredAccounts(rows){
 const q=$('user-search').value.trim().toLowerCase(),filter=$('monitor-filter').value,sort=$('monitor-sort').value;
 const list=rows.filter(u=>(u.username+' '+handle(u)+' '+(u.name||'')).toLowerCase().includes(q)&&(
  filter==='all'||filter==='enabled'&&u.monitored&&u.enabled||filter==='paused'&&u.monitored&&!u.enabled||
  filter==='archived'&&!u.monitored||filter==='attention'&&accountNeedsAttention(u)||
  filter==='visible'&&visibleInTimeline(u)||filter==='hidden'&&!visibleInTimeline(u)
 ));
 const byName=(a,b)=>handle(a).localeCompare(handle(b),'en',{sensitivity:'base'})||a.username.localeCompare(b.username);
 return list.sort((a,b)=>{
  if(sort.startsWith('posts-')){
   if(!!a.stats_pending!==!!b.stats_pending)return a.stats_pending?1:-1;
   return (sort==='posts-desc'?b.posts-a.posts:a.posts-b.posts)||byName(a,b);
  }
  if(sort==='recent')return (b.last_finished||b.last_run||0)-(a.last_finished||a.last_run||0)||byName(a,b);
  return byName(a,b);
 });
}
function accountCard(u){
 const key=escape(u.username),pending=accountPending.has(u.username),disabled=pending?' disabled':'';
 const [label,cls]=u.runtime_pending?['状态加载中','neutral']:u.monitored?monitorStatus(u):['仅保留归档','neutral'];
 const shown=visibleInTimeline(u),open=name=>accountExpanded.has(u.username+':'+name)?' open':'';
 const action=(op,text,extra='')=>`<button type="button" class="text-button ${extra}" data-monitor="${key}" data-op="${op}"${disabled}>${text}</button>`;
 let management=u.monitored?(u.runtime_pending?'<p class="muted">正在读取采集状态…</p>':`${collectionActions(u)}${action('edit','编辑采集设置')}${action('toggle',u.enabled?'暂停采集':'启用采集')}${u.last_error?.includes('请重置断点')?action('reset-progress','重置断点并重试'):''}${u.running?'':action('delete','取消监控','delete')}`):action('readd','重新添加监控');
 if(pending)management=management.replace(/<button\b([^>]*)>/g,(tag,attrs)=>attrs.includes('disabled')?tag:`<button disabled${attrs}>`);
 const detail=u.monitored&&!u.runtime_pending?`<details class="account-details" data-detail="${key}:status"${open('status')}><summary>采集详情${accountNeedsAttention(u)?' · 需关注':''}</summary><div>${baselineHTML(u)}${checkpointHTML(u)}${u.last_error?`<p class="account-error">${escape(u.last_error)}</p>`:''}${monitorNextStep(u)}<p>最近结束：${u.last_finished?escape(date(u.last_finished,true)):'尚未采集'}</p><p>${!u.enabled||state.overview?.paused?'自动采集已暂停':u.next_run>state.overview?.server_time?'下次：'+escape(date(u.next_run,true)):'等待调度'} · 每 ${number(u.interval_seconds/3600)} 小时</p><p>上轮新增 ${number(u.new_posts)} 条</p><a class="account-task-link" href="#activity">查看任务 →</a></div></details>`:'';
 return `<div class="account-card-top"><a class="account-identity" href="${userURL(u.username)}" aria-label="查看 ${escape(u.name||handle(u))} 的归档">${avatar(handle(u),'user-avatar',u.avatar_url)}<span><strong><span class="account-name" title="${escape(u.name||handle(u))}">${escape(u.name||handle(u))}</span>${u.protected?icon('lock'):''}</strong><span class="user-handle" title="@${escape(handle(u))}">@${escape(handle(u))}</span>${u.current_username&&u.current_username!==u.username?`<small class="muted" title="原 @${key} · 归档已延续">原 @${key} · 归档已延续</small>`:''}</span></a><span class="badge ${cls}">${label}</span></div>
 <div class="account-stats"><span><strong>${userStat(u,'posts')}</strong> 条推文</span><span><strong>${userStat(u,'media')}</strong> 个媒体</span><span class="account-last-saved">${u.stats_pending?'统计加载中…':u.last_saved?'最近归档 '+escape(date(u.last_saved,true)):'等待第一次采集'}</span></div>
 <div class="account-controls"><button type="button" class="timeline-switch" role="switch" aria-checked="${shown}" aria-label="@${escape(handle(u))} 在主时间线显示" data-monitor="${key}" data-op="timeline"${disabled}><span class="switch-track" aria-hidden="true"></span><span>首页${shown?'显示':'隐藏'}</span></button><details class="account-actions" data-detail="${key}:actions"${open('actions')}><summary aria-label="管理 @${escape(handle(u))}">管理</summary><div class="account-action-list">${management}</div></details></div>${detail}`;
}
function renderAccounts(){
 if(state.view!=='archive')return;
 const rows=accountRows(),list=filteredAccounts(rows),root=$('user-list');
 $('user-count').textContent=`显示 ${list.length} / ${rows.length} 个账号 · ${rows.filter(u=>u.monitored).length} 个监控 · ${rows.filter(u=>!u.monitored).length} 个仅归档`;
 $('user-empty').hidden=list.length>0;
 $('user-empty').querySelector('h3').textContent=rows.length?'没有匹配的账号':'开始你的第一份归档';
 $('user-empty').querySelector('p').textContent=rows.length?'试试其他名称或筛选条件。':'添加一个账号，即可开始采集与归档。';
 $('user-empty').querySelector('button').hidden=rows.length>0;
 const focused=document.activeElement?.closest('#user-list [data-op],#user-list summary,#user-list .account-identity');
 const focusKey=focused?{account:focused.closest('[data-account]').dataset.account,op:focused.dataset.op,detail:focused.parentElement.dataset.detail,identity:focused.classList.contains('account-identity')}:null;
 const existing=new Map([...root.children].map(el=>[el.dataset.account,el]));
 const wanted=new Set(list.map(u=>u.username));
 for(const [name,node] of existing)if(!wanted.has(name))node.remove();
 let cursor=root.firstElementChild;
 for(const u of list){
  let node=existing.get(u.username);
  if(!node){node=document.createElement('article');node.className='user-card account-card';node.dataset.account=u.username;}
  const html=accountCard(u);if(node._accountHTML!==html){node.innerHTML=html;node._accountHTML=html;}
  if(node!==cursor)root.insertBefore(node,cursor);
  cursor=node.nextElementSibling;
 }
 if(focusKey&&!focused.isConnected){
  const node=[...root.children].find(el=>el.dataset.account===focusKey.account);
  const target=focusKey.op?[...node?.querySelectorAll('[data-op]')||[]].find(el=>el.dataset.op===focusKey.op):focusKey.identity?node?.querySelector('.account-identity'):[...node?.querySelectorAll('details')||[]].find(el=>el.dataset.detail===focusKey.detail)?.querySelector('summary');
  target?.focus({preventScroll:true});
 }
}
$('user-list').addEventListener('toggle',event=>{
 const detail=event.target.dataset?.detail;if(!detail)return;
 if(event.target.open)accountExpanded.add(detail);else accountExpanded.delete(detail);
},true);
async function refreshAccountsAfterChange(){
 monitorRequest++;userRequest++;
 // Keep cached cards while fetching, but isolate responses from before the change.
 await Promise.all([refreshUsers('',true),refreshMonitors(),refreshOverview(true)]);
}
async function handleAccountAction(event){
 const b=event.target.closest('[data-op]');if(!b)return;
 const u=accountRows().find(u=>u.username===b.dataset.monitor);if(!u||accountPending.has(u.username))return;
 if(b.dataset.op==='edit'){await action(b,()=>openMonitor(u));return;}
 if(b.dataset.op==='readd'){await action(b,()=>openMonitor(u,true));return;}
 if(b.dataset.op==='delete'){state.deleting=u.username;$('confirm-text').textContent='取消 @'+handle(u)+' 的监控？已归档推文、媒体、收藏和首页显示偏好都会保留；排队任务将取消。';$('confirm-dialog').showModal();return;}
 if(b.dataset.op==='rescan'&&!window.confirm('将清除未完成的分页断点，并从最新内容重新扫描。已归档内容不会删除。是否继续？'))return;
 accountPending.add(u.username);renderUsers();
 try{
  const target=encodeURIComponent(u.username);
  switch(b.dataset.op){
   case 'timeline':await api('/users/'+target+'/timeline','PUT',{show_in_timeline:!visibleInTimeline(u)});toast(visibleInTimeline(u)?'已从首页隐藏，归档与采集不受影响':'已在首页显示');break;
   case 'toggle':await api('/monitors/'+target,'PUT',{...monitorPayload(u),enabled:!u.enabled});toast(u.enabled?'监控已暂停':'监控已启用');break;
   case 'reset-progress':await api('/monitors/'+target+'/reset-progress','POST');toast('断点已重置，等待采集');break;
   case 'stop':await api('/monitors/'+target+'/stop','POST');toast('已请求停止并暂停监控');break;
   case 'cancel-queue':{const result=await api('/activity/jobs/'+u.queued_job.id,'DELETE');toast(result.message);break;}
   case 'run':case 'backfill':case 'rescan':await api('/monitors/'+target+'/run','POST',{mode:b.dataset.op==='run'?'incremental':b.dataset.op});toast('采集已加入队列');break;
   default:return;
  }
  await refreshAccountsAfterChange();
 }catch(err){toast(err.message,true);}
 finally{accountPending.delete(u.username);renderUsers();}
}
