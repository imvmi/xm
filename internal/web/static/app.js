'use strict';
const $ = id => document.getElementById(id);
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const icon = name => `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
const state = {csrf:'', view:'archive', settings:null, monitors:[], overview:null, offset:0, total:0, editing:null, deleting:null, postRequest:0, authenticated:false, users:[], target:'', mediaTab:false, navRequest:0};
let toastTimer, searchTimer;
function toast(message, error=false) { clearTimeout(toastTimer); $('toast').textContent=message; $('toast').classList.toggle('error',error); $('toast').hidden=false; toastTimer=setTimeout(()=>$('toast').hidden=true,error?7000:4000); }
function date(value, short=false) { if(!value)return '尚未执行';const d=new Date(typeof value==='number'?value*1000:value);if(Number.isNaN(d.getTime()))return String(value);return d.toLocaleString('zh-CN',short?{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}:{year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}); }
function number(v) { return Number(v||0).toLocaleString('zh-CN'); }
function bytes(v) { if(!v)return '0 B';const units=['B','KB','MB','GB','TB'];const i=Math.min(Math.floor(Math.log(v)/Math.log(1024)),4);return `${(v/1024**i).toFixed(i>0?1:0)} ${units[i]}`; }
async function api(path, method='GET', data) {
  const options={method,credentials:'same-origin',headers:{}};
  if(method!=='GET'){options.headers['Content-Type']='application/json';options.headers['X-CSRF-Token']=state.csrf;options.body=JSON.stringify(data??{});}
  let res;try{res=await fetch('/api'+path,options);}catch{throw new Error('无法连接服务，请检查服务是否正在运行');}
  const body=await res.json().catch(()=>({error:'服务器返回了无法识别的响应'}));
  if(res.status===401&&path!=='/login'){showLogin();throw new Error('登录已过期，请重新登录');}
  if(!res.ok)throw new Error(body.error||'操作失败');return body;
}
async function action(button, work) { if(button?.disabled)return; if(button)button.disabled=true;try{await work();}catch(err){toast(err.message,true);}finally{if(button)button.disabled=false;} }
function showLogin() {
  state.authenticated=false;state.csrf='';state.postRequest++;
  $('app').hidden=true;$('login-view').hidden=false;
  for(const id of ['posts','user-list','profile-header','profile-summary','monitor-rows','runs'])$(id).replaceChildren();
  $('storage-profile-form').reset();$('notification-form').reset();$('notification-channels').replaceChildren();$('notification-deliveries').replaceChildren();
  $('large-image').removeAttribute('src');$('cookie-content').value='';$('proxy-url').value='';
  document.querySelectorAll('dialog[open]').forEach(d=>d.close());
  state.monitors=[];state.users=[];state.settings=null;state.overview=null;state.navRequest++;
}
async function enter() {
  state.authenticated=true;$('password').value='';$('login-error').textContent='';$('login-view').hidden=true;$('app').hidden=false;
  await refreshOverview();await navigate();
}
$('login-form').addEventListener('submit',async event=>{
  event.preventDefault();const button=event.submitter;button.disabled=true;$('login-error').textContent='';
  try{const r=await api('/login','POST',{password:$('password').value});state.csrf=r.csrf;await enter();}catch(err){$('login-error').textContent=err.message;}finally{button.disabled=false;}
});
$('logout').addEventListener('click',()=>action($('logout'),async()=>{await api('/logout','POST');showLogin();}));

async function refreshOverview() {
  const o=await api('/overview');state.overview=o;const c=o.counts;
  $('nav-count').textContent=number(c.posts);$('stat-posts').textContent=number(c.posts);$('stat-today').textContent=number(c.today);$('stat-monitors').textContent=number(c.monitors);$('stat-active').textContent=number(c.active);$('stat-media').textContent=number(c.media);$('stat-bytes').textContent=bytes(c.media_bytes);
  const cooling=Number(o.cooldown_until)>o.server_time;
  let label=o.running?`正在采集 @${o.running}`:o.cookie_blocked?'登录状态待处理':!o.cookie_ready?'等待导入 Cookie':cooling?'限流冷却中':o.paused?'自动轮询已暂停':'服务运行中';
  $('service-state').replaceChildren();const dot=document.createElement('span');dot.className='status-dot';$('service-state').append(dot,document.createTextNode(label));
  $('pause-all').textContent=o.paused?'恢复自动轮询':'暂停自动轮询';
  const banner=$('connection-banner');banner.hidden=true;banner.replaceChildren();
  let warning='';
  if(o.cookie_blocked)warning=`采集已暂停：${o.cookie_blocked}。请重新导入 Cookie。`;
  else if(!o.cookie_ready)warning='还没有可用的 Cookie。先完成连接设置，监控任务才能开始采集。';
  else if(cooling)warning=`X 请求限流，全部采集将在 ${date(Number(o.cooldown_until),true)} 后恢复。`;
  else if(o.scheduler_error)warning=o.scheduler_error;
  if(warning){banner.hidden=false;banner.append(document.createTextNode(warning+' '));const link=document.createElement('a');link.href='#settings';link.textContent='前往连接设置 →';banner.append(link);}
}
const titles={archive:'推文归档',user:'用户动态',monitors:'监控账号',activity:'采集记录',settings:'连接设置',notifications:'通知设置'};
function avatar(username, extra='') { const alt=[...username].reduce((sum,c)=>sum+c.charCodeAt(0),0)%4;return `<span class="avatar alt${alt} ${extra}">${escape(username.slice(0,2).toUpperCase())}</span>`; }
function userURL(username) { return '#user/'+encodeURIComponent(username); }
async function navigate() {
  if(!state.authenticated)return;
  const request=++state.navRequest;state.postRequest++;
  const next=location.hash.slice(1),match=/^user\/([A-Za-z0-9_]{1,15})$/.exec(next);
  state.view=match?'user':titles[next]&&next!=='user'?next:'archive';
  for(const name of Object.keys(titles))$('view-'+name).hidden=name!==state.view;
  for(const a of document.querySelectorAll('[data-view]'))a.classList.toggle('active',a.dataset.view===(state.view==='user'?'archive':state.view));
  $('breadcrumb-title').textContent=titles[state.view];document.title=`${titles[state.view]} · XM`;
  if(state.view==='archive')await refreshUsers();
  else if(state.view==='user') {
    state.target=match[1].toLowerCase();state.offset=0;state.mediaTab=false;$('search').value='';$('kind-filter').value='';
    $('posts').replaceChildren();$('profile-header').replaceChildren();$('profile-top-name').textContent='@'+state.target;$('profile-top-count').textContent='正在读取档案…';
    await refreshUsers();if(request!==state.navRequest)return;renderProfile();syncTabs();await refreshPosts();
  }
  else if(state.view==='monitors')await refreshMonitors();
  else if(state.view==='activity')await refreshRuns();
  else if(state.view==='settings')await refreshSettings();
  else if(state.view==='notifications')await refreshNotifications();
}
window.addEventListener('hashchange',()=>navigate().catch(err=>toast(err.message,true)));
async function refreshUsers(){
 const users=await api('/users');if(!state.authenticated)return;state.users=users;renderUsers();
}
function renderUsers(){
 const q=$('user-search').value.trim().toLowerCase();const users=state.users.filter(u=>(u.username+' '+u.name).toLowerCase().includes(q));
 $('user-count').textContent=users.length+' 位用户';$('user-empty').hidden=users.length>0;
 $('user-empty').querySelector('h3').textContent=q?'没有找到匹配的用户':'还没有用户档案';
 $('user-list').innerHTML=users.map(u=>`<a class="user-card" href="${userURL(u.username)}"><div class="user-card-top">${avatar(u.username,'user-avatar')}<span class="badge ${u.enabled?'':'neutral'}">${u.monitored?(u.enabled?'已启用监控':'监控已暂停'):'已归档'}</span></div><h3>${escape(u.name)}${u.protected?icon('lock'):''}</h3><div class="user-handle">@${escape(u.username)}</div><p class="user-bio">${escape(u.bio||'关注的动态，保存在自己的空间里。')}</p><div class="user-card-stats"><span><strong>${number(u.posts)}</strong> 条推文</span><span><strong>${number(u.media)}</strong> 个媒体</span></div><footer><span>${u.last_saved?'最近归档 '+escape(date(u.last_saved,true)):'等待第一次采集'}</span><span class="user-open">查看动态 →</span></footer></a>`).join('');
}
$('user-search').addEventListener('input',renderUsers);
function renderProfile(){
 const u=state.users.find(u=>u.username===state.target)||{username:state.target,name:state.target,posts:0,media:0};
 $('profile-top-name').textContent=u.name;$('profile-top-count').textContent=number(u.posts)+' 条已归档推文';document.title=u.name+' (@'+u.username+') · XM';
 $('profile-header').innerHTML=`<div class="profile-cover"><span>PERSONAL ARCHIVE</span>${icon('lock')}</div><div class="profile-identity"><div class="profile-actions">${avatar(u.username,'profile-avatar')}<a href="https://x.com/${encodeURIComponent(u.username)}" class="button secondary" target="_blank" rel="noopener noreferrer">在 X 查看 ${icon('arrow')}</a></div><h2>${escape(u.name)} ${u.protected?icon('lock'):''}</h2><div class="user-handle">@${escape(u.username)}</div><p class="profile-bio">${escape(u.bio||'这里保存着这个用户的动态，以及动态中引用的内容。')}</p><div class="profile-meta">${icon('clock')} ${u.last_saved?'最近归档于 '+escape(date(u.last_saved)):'尚未归档'}</div><div class="profile-numbers"><span><strong>${number(u.posts)}</strong> 条推文</span><span><strong>${number(u.media)}</strong> 个已归档媒体</span><span class="badge neutral">${u.monitored?(u.enabled?'已启用监控':'监控已暂停'):'已归档'}</span></div></div>`;
 $('profile-summary').innerHTML=`<dl><div><dt>已保存推文</dt><dd>${number(u.posts)}</dd></div><div><dt>已归档媒体（含引用）</dt><dd>${number(u.media)}</dd></div><div><dt>轮询周期</dt><dd>${u.interval_seconds?number(u.interval_seconds/60)+' 分钟':'未设置'}</dd></div><div><dt>最近采集</dt><dd>${escape(date(u.last_run,true))}</dd></div></dl>`;
}
function syncTabs(){for(const [id,active] of [['tab-posts',!state.mediaTab],['tab-media',state.mediaTab]]){$(id).classList.toggle('selected',active);$(id).setAttribute('aria-selected',String(active));}}
for(const [id,media] of [['tab-posts',false],['tab-media',true]])$(id).addEventListener('click',()=>action(null,async()=>{state.mediaTab=media;state.offset=0;$('kind-filter').value='';syncTabs();await refreshPosts();}));
function mediaHTML(post) {
  const done=post.media.filter(m=>m.status==='done');let html='';
  if(done.length){html+=`<div class="media-grid ${done.length===1?'single':''}">`;for(const m of done){const src=`/media/${encodeURIComponent(post.id)}/${encodeURIComponent(m.id)}`;if(m.kind==='photo')html+=`<button type="button" class="media-button" data-image="${escape(src)}" aria-label="放大 @${escape(post.username)} 的归档图片"><img src="${escape(src)}" loading="lazy" alt="@${escape(post.username)} 的归档图片"></button>`;else html+=`<video controls playsinline preload="metadata" src="${escape(src)}" aria-label="@${escape(post.username)} 的归档视频"></video>`;}html+='</div>';}
  for(const m of post.media.filter(m=>m.status!=='done'))html+=`<div class="media-pending">${icon('image')}<span>${m.kind==='photo'?'图片':'视频'}${m.status==='failed'?'下载失败':'等待下载'}${m.error?' · '+escape(m.error):''}</span></div>`;
  return html;
}
function quoteHTML(p,depth=0){
 if(!p.quote_id)return '';
 if(!p.quote||depth>=4)return `<div class="quote-unavailable">${icon('lock')}<span>引用内容暂不可用或尚未归档</span><a href="https://x.com/i/status/${encodeURIComponent(p.quote_id)}" target="_blank" rel="noopener noreferrer">查看原文 ↗</a></div>`;
 const q=p.quote;
 return `<div class="quote-card"><div class="quote-content"><div class="quote-author">${avatar(q.username,'tiny-quote')}<strong>${escape(q.name||q.username)}</strong><span>@${escape(q.username)}</span><span>· ${escape(date(q.created_at,true))}</span></div><div class="post-text quote-text">${escape(q.text)}</div>${q.text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(q)}${quoteHTML(q,depth+1)}</div><a class="quote-source" href="https://x.com/i/status/${encodeURIComponent(q.id)}" target="_blank" rel="noopener noreferrer">已归档的引用推文 ${icon('arrow')}</a></div>`;
}
async function refreshPosts() {
  if(state.view!=='user')return;
  const id=++state.postRequest;const q=new URLSearchParams({q:$('search').value,target:state.target,kind:$('kind-filter').value||(state.mediaTab?'media':''),offset:String(state.offset)});
  const r=await api('/posts?'+q);if(id!==state.postRequest||!state.authenticated||state.view!=='user')return;
  state.total=r.total;$('result-count').textContent=number(r.total);
  $('posts').innerHTML=r.items.map(p=>`<article class="post"><div class="post-body"><div class="post-header">${avatar(p.username)}<div class="timeline-author"><strong>${escape(p.name||p.username)}</strong><span>@${escape(p.username)} · ${escape(date(p.created_at,true))}</span></div>${p.repost_id?'<span class="badge neutral">转推</span>':p.reply_to?'<span class="badge neutral">回复</span>':''}</div><div class="post-main"><div class="post-text">${escape(p.text)}</div>${p.text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(p)}${quoteHTML(p)}</div></div><footer class="post-footer"><span>${icon('clock')}${escape(date(p.saved_at,true))} 归档</span><a href="https://x.com/i/status/${encodeURIComponent(p.id)}" target="_blank" rel="noopener noreferrer">查看原推文${icon('arrow')}</a></footer></article>`).join('');
  $('post-empty').hidden=r.items.length>0;
  const filtered=$('search').value||$('kind-filter').value||state.mediaTab;
  $('post-empty').querySelector('h3').textContent=filtered?'没有找到匹配的动态':'还没有保存的推文';
  $('empty-text').textContent=filtered?'搜索也会匹配引用推文；媒体筛选包含引用中的图片和视频。':'采集完成后，该用户的推文及引用内容会显示在这里。';
  $('page-label').textContent=r.total?`第 ${state.offset+1}–${Math.min(state.offset+r.limit,r.total)} 条，共 ${number(r.total)} 条`:'暂无归档内容';
  $('prev-page').disabled=state.offset===0;$('next-page').disabled=state.offset+r.limit>=r.total;
}
$('posts').addEventListener('click',event=>{
  const image=event.target.closest('[data-image]');if(image){$('large-image').src=image.dataset.image;$('image-dialog').showModal();}
  if(event.target.classList.contains('expand-text')){const text=event.target.previousElementSibling;const expanded=text.classList.toggle('expanded');event.target.textContent=expanded?'收起全文':'展开全文';}
});
$('image-dialog').addEventListener('close',()=>$('large-image').removeAttribute('src'));
$('search').addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>{state.offset=0;refreshPosts().catch(e=>toast(e.message,true));},300);});
for(const id of ['kind-filter'])$(id).addEventListener('change',()=>{state.offset=0;refreshPosts().catch(e=>toast(e.message,true));});
$('refresh-archive').addEventListener('click',()=>action($('refresh-archive'),async()=>{await refreshOverview();await refreshUsers();}));
$('refresh-user').addEventListener('click',()=>action($('refresh-user'),async()=>{await refreshOverview();await refreshUsers();renderProfile();await refreshPosts();}));
for(const [id,delta] of [['prev-page',-24],['next-page',24]])$(id).addEventListener('click',()=>action(null,async()=>{state.offset=Math.max(0,state.offset+delta);await refreshPosts();}));

function monitorStatus(m) { if(m.running)return ['采集中','running'];if(m.requested)return ['等待执行','warn'];if(!m.enabled)return ['已暂停','neutral'];if(state.overview?.cookie_blocked)return ['需更新 Cookie','warn'];if(!state.overview?.cookie_ready)return ['等待 Cookie','warn'];if(Number(state.overview?.cooldown_until)>state.overview?.server_time)return ['限流冷却','warn'];if(state.overview?.paused)return ['全局已暂停','neutral'];if(m.last_error)return ['等待重试','error'];return ['监控中','']; }
async function refreshMonitors() {
  const list=await api('/monitors');state.monitors=list;$('monitor-empty').hidden=list.length>0;
  $('monitor-rows').innerHTML=list.map(m=>{const [label,cls]=monitorStatus(m);return `<tr><td><div class="user-cell"><span class="avatar">${escape(m.username.slice(0,2).toUpperCase())}</span><div><strong>@${escape(m.username)}</strong><div class="table-sub">每轮 ${m.limit} 条${m.replies?' · 含回复':''}</div></div></div></td><td>${number(m.interval_seconds/60)} 分钟</td><td><span class="badge ${cls}">${label}</span>${m.last_error?`<div class="table-sub error">${escape(m.last_error)}</div>`:''}</td><td><div>${escape(date(m.last_finished,true))}</div><div class="table-sub">${m.enabled?'计划 '+escape(date(m.next_run,true)):'自动轮询已暂停'}</div></td><td><strong>${number(m.posts)}</strong><div class="table-sub">上轮新增 ${m.new_posts} 条</div></td><td><div class="row-actions"><button class="text-button" data-monitor="${m.username}" data-op="${m.running||m.requested?'stop':'run'}">${m.running?'停止':m.requested?'取消排队':'立即采集'}</button><button class="text-button" data-monitor="${m.username}" data-op="edit">编辑</button><button class="text-button" data-monitor="${m.username}" data-op="toggle">${m.enabled?'暂停':'启用'}</button><button class="text-button delete" data-monitor="${m.username}" data-op="delete" ${m.running?'disabled':''}>移除</button></div></td></tr>`;}).join('');
}
function monitorPayload(m) { return {username:m.username,interval_seconds:m.interval_seconds,limit:m.limit,replies:m.replies,enabled:m.enabled}; }
async function openMonitor(m=null) {
  if(!state.settings)state.settings=await api('/settings');state.editing=m?.username||null;
  $('monitor-dialog-title').textContent=m?'编辑监控账号':'添加监控账号';$('monitor-username').value=m?.username||'';$('monitor-username').disabled=!!m;
  $('monitor-interval').value=(m?.interval_seconds||state.settings.default_interval_seconds)/60;$('monitor-limit').value=m?.limit||100;$('monitor-replies').checked=!!m?.replies;$('monitor-enabled').checked=m?m.enabled:true;$('monitor-dialog').showModal();
}
document.querySelectorAll('[data-action="add"]').forEach(b=>b.addEventListener('click',()=>action(b,()=>openMonitor())));
document.querySelectorAll('[data-close]').forEach(b=>b.addEventListener('click',()=>$(b.dataset.close).close()));
$('monitor-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const payload={username:$('monitor-username').value.trim(),interval_seconds:Math.round(Number($('monitor-interval').value)*60),limit:Number($('monitor-limit').value),replies:$('monitor-replies').checked,enabled:$('monitor-enabled').checked};
  await api(state.editing?'/monitors/'+encodeURIComponent(state.editing):'/monitors',state.editing?'PUT':'POST',payload);$('monitor-dialog').close();toast('监控账号已保存');await refreshOverview();if(state.view==='monitors')await refreshMonitors();else location.hash='monitors';
});});
$('monitor-rows').addEventListener('click',event=>{const b=event.target.closest('[data-op]');if(!b)return;const m=state.monitors.find(m=>m.username===b.dataset.monitor);if(!m)return;action(b,async()=>{
  switch(b.dataset.op){
    case 'edit':await openMonitor(m);return;
    case 'delete':state.deleting=m.username;$('confirm-text').textContent='将停止监控 @'+m.username+'。';$('confirm-dialog').showModal();return;
    case 'toggle':await api('/monitors/'+m.username,'PUT',{...monitorPayload(m),enabled:!m.enabled});toast(m.enabled?'监控已暂停':'监控已启用，将按周期执行');break;
    case 'run':await api('/monitors/'+m.username+'/run','POST');toast('已加入采集队列');break;
    case 'stop':await api('/monitors/'+m.username+'/stop','POST');toast('已请求停止并暂停此账号');break;
  }
  await refreshMonitors();await refreshOverview();
});});
$('confirm-delete').addEventListener('click',()=>action($('confirm-delete'),async()=>{await api('/monitors/'+encodeURIComponent(state.deleting),'DELETE');$('confirm-dialog').close();toast('监控已移除，归档内容保留');await refreshMonitors();await refreshOverview();}));
$('pause-all').addEventListener('click',()=>action($('pause-all'),async()=>{const paused=!state.overview?.paused;await api('/settings','PUT',{polling_paused:paused});toast(paused?'已暂停自动轮询，当前任务继续执行':'已恢复自动轮询');await refreshOverview();await refreshMonitors();}));

async function refreshSettings() {
  const s=await api('/settings');state.settings=s;
  $('cookie-badge').textContent=s.cookie_blocked?'需更新':s.cookie_ready?'已保存':'未导入';$('cookie-badge').className='badge '+(s.cookie_blocked?'error':s.cookie_ready?'':'neutral');
  $('cookie-status').textContent=s.cookie_blocked|| (s.cookie_ready?`Cookie 已加密保存${s.cookie_imported?' · '+date(s.cookie_imported)+' 更新':''}。有效性以采集结果为准。`:'支持粘贴 Cookie 请求头，或导入 JSON / Netscape 文件。');
  $('proxy-mode').value=s.proxy_configured?'proxy':'direct';$('proxy-url').value='';$('proxy-label').textContent='当前：'+s.proxy_label;$('proxy-fields').hidden=!s.proxy_configured;$('default-interval').value=s.default_interval_seconds/60;
  await refreshStorage();
}
$('proxy-mode').addEventListener('change',()=>$('proxy-fields').hidden=$('proxy-mode').value==='direct');
$('cookie-file').addEventListener('change',async()=>{const file=$('cookie-file').files[0];if(!file)return;if(file.size>1024*1024){toast('Cookie 文件不能超过 1 MiB',true);$('cookie-file').value='';return;}try{$('cookie-content').value=await file.text();$('cookie-file-name').textContent=file.name;}catch{toast('无法读取文件',true);}});
$('cookie-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{const content=$('cookie-content').value.trim();if(!content)throw new Error('请粘贴 Cookie 或选择文件');const r=await api('/cookies','POST',{content});$('cookie-content').value='';$('cookie-file').value='';$('cookie-file-name').textContent='或直接粘贴到上方';toast(r.message);await refreshSettings();await refreshOverview();});});
function proxyChange() { if($('proxy-mode').value==='direct')return {proxy:''};const p=$('proxy-url').value.trim();if(p)return {proxy:p};if(!state.settings?.proxy_configured)throw new Error('请输入代理地址');return {}; }
$('settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{await api('/settings','PUT',{...proxyChange(),default_interval_seconds:Math.round(Number($('default-interval').value)*60)});toast('设置已保存，下一个采集任务生效');await refreshSettings();});});
$('test-proxy').addEventListener('click',()=>action($('test-proxy'),async()=>{$('proxy-result').textContent='正在连接 X…';try{const r=await api('/proxy-test','POST',proxyChange());$('proxy-result').textContent=`收到 HTTP ${r.status} · ${r.latency_ms} ms。${r.message}`;}catch(err){$('proxy-result').textContent=err.message;throw err;}}));
const runLabels={running:['采集中','running'],bounded_complete:['已完成',''],failed:['采集失败','error'],media_failed:['媒体未完成','warn'],cancelled:['已停止','neutral'],interrupted:['已中断','warn']};
const stopLabels={limit_reached:'已达到本轮条数上限',max_scrolls_reached:'已达到本轮滚动上限',no_new_posts_observed:'本轮未再发现新内容'};
async function refreshRuns() {
  const runs=await api('/runs');$('runs').innerHTML=runs.length?runs.map(r=>{const [label,cls]=runLabels[r.status]||[r.status,'neutral'];return `<article class="run-row"><span class="avatar">${escape(r.target.slice(0,2).toUpperCase())}</span><div class="run-main"><strong>@${escape(r.target)}</strong><p>本轮读取 ${r.posts} 条 · ${escape(stopLabels[r.message]||r.message||'正在读取时间线')}</p><div class="run-time">${escape(date(r.started_at))}${r.finished_at?' → '+escape(date(r.finished_at,true)):''}</div></div><span class="badge ${cls}">${escape(label)}</span></article>`;}).join(''):'<div class="empty-state"><h3>还没有采集记录</h3><p>添加监控账号后，每次任务的状态都会记录在这里。</p></div>';
}
$('refresh-runs').addEventListener('click',()=>action($('refresh-runs'),refreshRuns));
let refreshing=false;
setInterval(async()=>{if(!state.authenticated||document.hidden||refreshing)return;refreshing=true;try{await refreshOverview();if(state.view==='monitors')await refreshMonitors();if(state.view==='activity')await refreshRuns();if(state.view==='notifications')await refreshNotifications(false);}catch(err){$('service-state').textContent='连接暂时中断';}finally{refreshing=false;}},6000);
(async()=>{try{const r=await api('/session');state.csrf=r.csrf;await enter();}catch{showLogin();}})();
