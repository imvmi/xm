'use strict';
let accountScroll=0,accountRestoring=false,accountMenuTarget=null;
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
 const shown=visibleInTimeline(u),enabled=!!u.monitored&&!!u.enabled;
 const autoHelp=!u.monitored?'仅保留归档；请在管理菜单中重新添加监控':u.runtime_pending?'正在读取采集状态':enabled?'关闭将停止当前采集并取消排队；归档保留':state.overview?.paused?'启用后仍需在采集设置中恢复全局自动采集':'按采集设置中的统一周期自动采集';
 const lastSaved=u.stats_pending?'统计加载中…':u.last_saved?'最近归档 '+date(u.last_saved,true):'等待第一次采集';
 return `<div class="account-card-top"><div class="account-identity"><a class="account-avatar-link" href="${userURL(u.username)}" aria-label="查看 ${escape(u.name||handle(u))} 的归档">${avatar(handle(u),'user-avatar',u.avatar_url)}</a><span><strong><a class="account-name" href="${userURL(u.username)}" title="${escape(u.name||handle(u))}">${escape(u.name||handle(u))}</a>${u.protected?icon('lock'):''}</strong><a class="user-handle" href="https://x.com/${encodeURIComponent(handle(u))}" target="_blank" rel="noopener noreferrer" title="@${escape(handle(u))}">@${escape(handle(u))}</a>${u.current_username&&u.current_username!==u.username?`<small class="muted" title="原 @${key} · 归档已延续">原 @${key} · 归档已延续</small>`:''}</span></div><span class="badge ${cls}">${label}${accountNeedsAttention(u)&&label!=='需关注'?' · 需关注':''}</span></div>
 <div class="account-stats"><span><strong>${userStat(u,'posts')}</strong> 条推文</span><span><strong>${userStat(u,'media')}</strong> 个媒体</span><span class="account-last-saved"><span title="${escape(lastSaved)}">${escape(lastSaved)}</span></span></div>
 <div class="account-controls"><button type="button" class="timeline-switch" role="switch" aria-checked="${shown}" aria-label="@${escape(handle(u))} 在主时间线显示" data-monitor="${key}" data-op="timeline"${disabled}><span class="switch-track" aria-hidden="true"></span><span>首页显示</span></button><button type="button" class="timeline-switch" role="switch" aria-checked="${enabled}" aria-label="@${escape(handle(u))} 自动采集" title="${escape(autoHelp)}" data-monitor="${key}" data-op="toggle"${pending||!u.monitored||u.runtime_pending?' disabled':''}><span class="switch-track" aria-hidden="true"></span><span>自动采集</span></button></div>`;
}
function accountManagementHTML(u){
 const key=escape(u.username),action=(op,text,extra='')=>`<button type="button" class="text-button ${extra}" data-monitor="${key}" data-op="${op}">${text}</button>`;
 let operations=u.monitored?(u.runtime_pending?'<p class="muted">正在读取采集状态…</p>':`${collectionActions(u)}${action('edit','配置编辑')}${u.last_error?.includes('请重置断点')?action('reset-progress','重置断点并重试'):''}`):action('readd','重新添加监控');
 if(accountPending.has(u.username))operations=operations.replace(/<button\b([^>]*)>/g,(tag,attrs)=>attrs.includes('disabled')?tag:`<button disabled${attrs}>`);
 const detail=u.monitored&&!u.runtime_pending?`<details class="account-details" data-detail="${key}:status"${accountExpanded.has(u.username+':status')?' open':''}><summary>采集详情${accountNeedsAttention(u)?' · 需关注':''}</summary><div>${baselineHTML(u)}${checkpointHTML(u)}${u.last_error?`<p class="account-error">${escape(u.last_error)}</p>`:''}${monitorNextStep(u)}<p>最近结束：${u.last_finished?escape(date(u.last_finished,true)):'尚未采集'}</p><p>${!u.enabled||state.overview?.paused?'自动采集已暂停':u.next_run>state.overview?.server_time?'下次：'+escape(date(u.next_run,true)):'等待调度'} · 每 ${number(u.interval_seconds/3600)} 小时</p><p>上轮新增 ${number(u.new_posts)} 条</p><a class="account-task-link" href="#activity">查看任务 →</a></div></details>`:'';
 const help=!u.monitored?'此账号仅保留归档；重新添加监控后可启用自动采集。':state.overview?.paused?'全局自动采集已暂停；账号开关开启也不会自动运行，可前往采集设置恢复。':'自动采集按统一周期运行；关闭开关会停止当前采集并取消排队，归档保留。';
 return `<p class="account-menu-help">${escape(help)}</p><div class="account-action-list">${operations}</div>${detail}`;
}
function renderAccountMenu(){
 const dialog=$('account-menu-dialog');if(!dialog.open)return;
 const u=accountRows().find(u=>u.username===accountMenuTarget);
 if(!u){dialog.close();return;}
 $('account-menu-name').textContent=u.name||handle(u);$('account-menu-name').href=userURL(u.username);$('account-menu-handle').textContent='@'+handle(u);$('account-menu-handle').href='https://x.com/'+encodeURIComponent(handle(u));
 const root=$('account-menu-content'),html=accountManagementHTML(u);
 if(root._accountHTML===html)return;
 const focused=root.contains(document.activeElement)?document.activeElement:null,op=focused?.dataset.op,summary=focused?.tagName==='SUMMARY';
 root.innerHTML=html;root._accountHTML=html;
 if(focused)(op?[...root.querySelectorAll('[data-op]')].find(b=>b.dataset.op===op):summary?root.querySelector('summary'):root.querySelector('a'))?.focus({preventScroll:true});
}
function openAccountMenu(username){
 if(state.view!=='archive'||!accountRows().some(u=>u.username===username))return;
 accountMenuTarget=username;archiveOverview.hidePopover();
 const opener=[...$('user-list').children].find(el=>el.dataset.account===username);
 opener?.focus({preventScroll:true});
 $('account-menu-dialog').showModal();renderAccountMenu();
}
function renderAccounts(){
 if(state.view!=='archive')return;
 const rows=accountRows(),list=filteredAccounts(rows),root=$('user-list');
 $('user-count').textContent=`显示 ${list.length} / ${rows.length} 个账号 · ${rows.filter(u=>u.monitored).length} 个监控 · ${rows.filter(u=>!u.monitored).length} 个仅归档`;
 $('user-empty').hidden=list.length>0;
 $('user-empty').querySelector('h3').textContent=rows.length?'没有匹配的账号':'开始你的第一份归档';
 $('user-empty').querySelector('p').textContent=rows.length?'试试其他名称或筛选条件。':'添加一个账号，即可开始采集与归档。';
 $('user-empty').querySelector('button').hidden=rows.length>0;
 const focused=document.activeElement?.closest('#user-list [data-op],#user-list summary,#user-list .account-identity a');
 const focusKey=focused?{account:focused.closest('[data-account]').dataset.account,op:focused.dataset.op,detail:focused.parentElement.dataset.detail,identity:focused.closest('.account-identity')?focused.getAttribute('href'):null}:null;
 const existing=new Map([...root.children].map(el=>[el.dataset.account,el]));
 const wanted=new Set(list.map(u=>u.username));
 for(const [name,node] of existing)if(!wanted.has(name))node.remove();
 let cursor=root.firstElementChild;
 for(const u of list){
  let node=existing.get(u.username);
  if(!node){node=document.createElement('article');node.className='user-card account-card';node.dataset.account=u.username;node.tabIndex=0;node.setAttribute('aria-haspopup','dialog');node.setAttribute('aria-controls','account-menu-dialog');}
  node.setAttribute('aria-label','管理 @'+handle(u));
  const html=accountCard(u);if(node._accountHTML!==html){node.innerHTML=html;node._accountHTML=html;}
  if(node!==cursor)root.insertBefore(node,cursor);
  cursor=node.nextElementSibling;
 }
 if(focusKey&&!focused.isConnected){
  const node=[...root.children].find(el=>el.dataset.account===focusKey.account);
  const target=focusKey.op?[...node?.querySelectorAll('[data-op]')||[]].find(el=>el.dataset.op===focusKey.op):focusKey.identity?[...node?.querySelectorAll('.account-identity a')||[]].find(el=>el.getAttribute('href')===focusKey.identity):[...node?.querySelectorAll('details')||[]].find(el=>el.dataset.detail===focusKey.detail)?.querySelector('summary');
  target?.focus({preventScroll:true});
 }
 renderAccountMenu();
}
$('account-menu-content').addEventListener('toggle',event=>{
 if(!event.target.isConnected)return;
 const detail=event.target.dataset?.detail;if(!detail)return;
 if(event.target.open)accountExpanded.add(detail);else accountExpanded.delete(detail);
},true);
$('user-list').addEventListener('click',event=>{
 if(event.target.closest('a,button,input,select,details')||window.getSelection()?.isCollapsed===false)return;
 const card=event.target.closest('[data-account]');if(card)openAccountMenu(card.dataset.account);
});
$('user-list').addEventListener('keydown',event=>{
 if(!event.target.matches('.account-card')||!['Enter',' '].includes(event.key))return;
 event.preventDefault();openAccountMenu(event.target.dataset.account);
});
$('account-menu-content').addEventListener('click',event=>{
 if(event.target.closest('a')){$('account-menu-dialog').close();return;}
 handleAccountAction(event);
});
$('account-menu-dialog').addEventListener('click',event=>{
 if(event.target!==event.currentTarget)return;
 const r=event.currentTarget.getBoundingClientRect();
 if(event.clientX<r.left||event.clientX>r.right||event.clientY<r.top||event.clientY>r.bottom)event.currentTarget.close();
});
$('account-menu-dialog').addEventListener('close',()=>{
 if($('account-menu-dialog').open)return;
 const target=accountMenuTarget;accountMenuTarget=null;
 if(state.authenticated&&state.view==='archive'&&!document.querySelector('dialog[open]')){
  [...$('user-list').children].find(el=>el.dataset.account===target)?.focus({preventScroll:true});
 }
});
window.addEventListener('hashchange',()=>{$('account-menu-dialog').close();});
async function refreshAccountsAfterChange(){
 monitorRequest++;userRequest++;
 // Keep cached cards while fetching, but isolate responses from before the change.
 await Promise.all([refreshUsers('',true),refreshMonitors(),refreshOverview(true)]);
}
async function handleAccountAction(event){
 const b=event.target.closest('[data-op]');if(!b)return;
 if(b.disabled)return;
 const u=accountRows().find(u=>u.username===b.dataset.monitor);if(!u)return;
 if(accountPending.has(u.username))return;
 if(b.closest('#account-menu-dialog'))$('account-menu-dialog').close();
 if(b.dataset.op==='edit'){await action(b,()=>openMonitor(u));return;}
 if(b.dataset.op==='readd'){await action(b,()=>openMonitor(u,true));return;}
 if(b.dataset.op==='rescan'&&!window.confirm('重新扫描将清除未完成的分页断点，并从最新内容重新扫描。已归档内容不会删除。是否继续？'))return;
 accountPending.add(u.username);renderUsers();
 try{
  const target=encodeURIComponent(u.username);
  switch(b.dataset.op){
   case 'timeline':await api('/users/'+target+'/timeline','PUT',{show_in_timeline:!visibleInTimeline(u)});toast(visibleInTimeline(u)?'已从首页隐藏，归档与采集不受影响':'已在首页显示');break;
   case 'toggle':await api('/monitors/'+target,'PUT',{replies:!!u.replies,enabled:!u.enabled});toast(u.enabled?'已关闭自动采集':state.overview?.paused?'账号自动采集已开启；全局仍暂停，请在采集设置中恢复':'已开启自动采集');break;
   case 'reset-progress':await api('/monitors/'+target+'/reset-progress','POST');toast('断点已重置，等待采集');break;
   case 'stop':await api('/monitors/'+target+'/stop','POST');toast('已请求停止并暂停监控');break;
   case 'cancel-queue':{const result=await api('/activity/jobs/'+u.queued_job.id,'DELETE');toast(result.message);break;}
   case 'run':case 'backfill':case 'rescan':await api('/monitors/'+target+'/run','POST',{mode:b.dataset.op==='run'?'incremental':b.dataset.op});toast(({run:'动态更新',backfill:'历史补采',rescan:'重新扫描'}[b.dataset.op])+'已加入队列');break;
   default:return;
  }
  await refreshAccountsAfterChange();
 }catch(err){toast(err.message,true);}
 finally{accountPending.delete(u.username);renderUsers();}
}
