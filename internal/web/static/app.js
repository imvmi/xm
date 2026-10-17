'use strict';
const $ = id => document.getElementById(id);
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const icon = name => `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
const state = {csrf:'', view:'archive', settings:null, monitors:[], overview:null, offset:0, total:0, editing:null, deleting:null, postRequest:0, authenticated:false, users:[], target:'', timelineTab:'posts', postsLoading:false, postsError:false, hasMore:false, postIDs:new Set(), navRequest:0};
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
  state.authenticated=false;state.csrf='';state.postRequest++;document.title='登录 · XM';
  $('app').hidden=true;$('login-view').hidden=false;
  for(const id of ['posts','user-list','profile-header','profile-summary','monitor-rows','runs'])$(id).replaceChildren();
  $('notification-form').reset();$('notification-channels').replaceChildren();$('notification-deliveries').replaceChildren();
  $('large-image').removeAttribute('src');$('cookie-content').value='';$('proxy-url').value='';
  document.querySelectorAll('dialog[open]').forEach(d=>d.close());
  $('notification-editor').hidden=true;state.monitors=[];state.users=[];state.settings=null;state.overview=null;state.navRequest++;
}
async function enter() {
  state.authenticated=true;$('password').value='';$('login-error').textContent='';$('login-view').hidden=true;$('app').hidden=false;
  await refreshOverview();await navigate();
}
$('login-form').addEventListener('submit',async event=>{
  event.preventDefault();const button=event.submitter;button.disabled=true;$('login-error').textContent='';
  try{const r=await api('/login','POST',{username:$('username').value,password:$('password').value});state.csrf=r.csrf;await enter();}catch(err){$('login-error').textContent=err.message;}finally{button.disabled=false;}
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
function avatar(username, extra='', source='') { const initials=escape(username.slice(0,2).toUpperCase()),alt=[...username].reduce((sum,c)=>sum+c.charCodeAt(0),0)%4;return `<span class="avatar alt${alt} ${extra}"><span class="avatar-fallback">${initials}</span>${source?`<img class="avatar-image" src="${escape(source)}" alt="" loading="lazy">`:''}</span>`; }
document.addEventListener('error',event=>{if(event.target instanceof HTMLImageElement&&event.target.classList.contains('avatar-image'))event.target.remove();},true);
function handle(u){return u.current_username||u.username;}
function userURL(username) { return '#user/'+encodeURIComponent(username); }
async function navigate() {
  if(!state.authenticated)return;
  const request=++state.navRequest;state.postRequest++;state.hasMore=false;state.postsLoading=false;clearTimeout(searchTimer);timelineObserver?.disconnect();
  const next=location.hash.slice(1),match=/^user\/([A-Za-z0-9_]{1,15})$/.exec(next);
  state.view=match?'user':titles[next]&&next!=='user'?next:'archive';
  for(const name of Object.keys(titles))$('view-'+name).hidden=name!==state.view;
  for(const a of document.querySelectorAll('[data-view]')){const active=a.dataset.view===(state.view==='user'?'archive':state.view);a.classList.toggle('active',active);if(active)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');}window.scrollTo(0,0);
  $('breadcrumb-title').textContent=titles[state.view];document.title=`${titles[state.view]} · XM`;
  if(state.view==='archive')await refreshUsers();
  else if(state.view==='user') {
    state.target=match[1].toLowerCase();state.offset=0;state.timelineTab='posts';$('search').value='';$('kind-filter').value='';
    $('posts').replaceChildren();$('profile-header').replaceChildren();$('profile-top-name').textContent='@'+state.target;$('profile-top-count').textContent='正在读取档案…';
    await refreshUsers();if(request!==state.navRequest)return;renderProfile();syncTabs();await refreshPosts();
  }
  else if(state.view==='monitors')await refreshMonitors();
  else if(state.view==='activity')await refreshRuns();
  else if(state.view==='settings')await refreshSettings();
  else if(state.view==='notifications')await refreshNotifications();
}
document.querySelector('.skip-link').addEventListener('click',event=>{event.preventDefault();(state.authenticated?$('main-content'):$('username')).focus();});
window.addEventListener('hashchange',()=>navigate().catch(err=>toast(err.message,true)));
async function refreshUsers(){
 const users=await api('/users');if(!state.authenticated)return;state.users=users;renderUsers();
}
function renderUsers(){
 const q=$('user-search').value.trim().toLowerCase();const users=state.users.filter(u=>(u.username+' '+handle(u)+' '+u.name).toLowerCase().includes(q));
 $('user-count').textContent=users.length+' 位用户';$('user-empty').hidden=users.length>0;
 $('user-empty').querySelector('h3').textContent=q?'没有找到匹配的用户':'开始你的第一份归档';
 $('user-empty').querySelector('p').textContent=q?'换个名称或用户名试试。':'添加一个监控账号，采集的动态会保存在这里。';
 $('user-empty').querySelector('button').hidden=!!q;
 $('user-list').innerHTML=users.map(u=>`<a class="user-card" href="${userURL(u.username)}"><div class="user-card-top">${avatar(handle(u),'user-avatar',u.avatar_url)}<div class="user-card-identity"><h3>${escape(u.name||handle(u))}${u.protected?icon('lock'):''}</h3><div class="user-handle">@${escape(handle(u))}</div></div><span class="user-open" aria-hidden="true">→</span></div><div class="user-card-stats"><span><strong>${number(u.posts)}</strong> 条推文</span><span><strong>${number(u.media)}</strong> 个媒体</span></div><footer><span class="badge ${u.enabled?'':'neutral'}">${u.monitored?(u.enabled?'监控中':'已暂停'):'已归档'}</span><span>${u.last_saved?'最近归档 '+escape(date(u.last_saved,true)):'等待第一次采集'}</span></footer></a>`).join('');
}
$('user-search').addEventListener('input',renderUsers);
function renderProfile(){
 const u=state.users.find(u=>u.username===state.target)||{username:state.target,name:state.target,posts:0,media:0};
 $('profile-top-name').textContent=u.name;$('profile-top-count').textContent=number(u.posts)+' 条已归档推文';document.title=u.name+' (@'+handle(u)+') · XM';
 $('profile-header').innerHTML=`<div class="profile-cover" aria-hidden="true"></div><div class="profile-identity"><div class="profile-actions">${avatar(handle(u),'profile-avatar',u.avatar_url)}<a href="https://x.com/${encodeURIComponent(handle(u))}" class="button secondary" target="_blank" rel="noopener noreferrer">在 X 查看 ${icon('arrow')}</a></div><h2>${escape(u.name)} ${u.protected?icon('lock'):''}</h2><div class="user-handle">@${escape(handle(u))}</div>${u.bio?`<p class="profile-bio">${escape(u.bio)}</p>`:''}<div class="profile-meta">${icon('clock')} ${u.last_saved?'最近归档于 '+escape(date(u.last_saved)):'尚未归档'}</div><div class="profile-numbers"><span><strong>${number(u.posts)}</strong> 条推文</span><span><strong>${number(u.media)}</strong> 个已归档媒体</span><span class="badge neutral">${u.monitored?(u.enabled?'已启用监控':'监控已暂停'):'已归档'}</span></div></div>`;
 $('profile-summary').innerHTML=`<dl><div><dt>已保存推文</dt><dd>${number(u.posts)}</dd></div><div><dt>已归档媒体（含引用）</dt><dd>${number(u.media)}</dd></div><div><dt>统一轮询周期</dt><dd>${u.interval_seconds?number(u.interval_seconds/60)+' 分钟':'未设置'}</dd></div><div><dt>最近采集</dt><dd>${escape(date(u.last_run,true))}</dd></div></dl>`;
}
const timelineTabs=['posts','replies','media'];
function syncTabs(){
 for(const name of timelineTabs){const selected=state.timelineTab===name;const tab=$('tab-'+name);tab.classList.toggle('selected',selected);tab.setAttribute('aria-selected',String(selected));tab.tabIndex=selected?0:-1;}
 $('timeline-panel').setAttribute('aria-labelledby','tab-'+state.timelineTab);
 $('kind-filter').querySelector('option[value="text"]').disabled=state.timelineTab==='media';
}
for(const name of timelineTabs){
 const tab=$('tab-'+name);
 tab.addEventListener('click',()=>{if(state.timelineTab===name)return;state.timelineTab=name;$('kind-filter').value='';syncTabs();refreshPosts();});
 tab.addEventListener('keydown',event=>{if(event.ctrlKey||event.metaKey||event.altKey)return;let i=timelineTabs.indexOf(name);if(event.key==='ArrowRight')i=(i+1)%3;else if(event.key==='ArrowLeft')i=(i+2)%3;else if(event.key==='Home')i=0;else if(event.key==='End')i=2;else return;event.preventDefault();$('tab-'+timelineTabs[i]).focus();$('tab-'+timelineTabs[i]).click();});
}
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
 return `<div class="quote-card"><div class="quote-content"><div class="quote-author">${avatar(q.username,'tiny-quote',state.users?.find(u=>(u.current_username||u.username)===q.username)?.avatar_url)}<strong>${escape(q.name||q.username)}</strong><span>@${escape(q.username)}</span><span>· ${escape(date(q.created_at,true))}</span></div><div class="post-text quote-text">${escape(q.text)}</div>${q.text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(q)}${quoteHTML(q,depth+1)}</div><a class="quote-source" href="https://x.com/i/status/${encodeURIComponent(q.id)}" target="_blank" rel="noopener noreferrer">已归档的引用推文 ${icon('arrow')}</a></div>`;
}
function postHTML(p){return `<article class="post"><div class="post-body">${avatar(p.username,'post-avatar',state.users?.find(u=>(u.current_username||u.username)===p.username)?.avatar_url)}<div class="post-content"><div class="post-header"><div class="timeline-author"><strong>${escape(p.name||p.username)}</strong><span class="timeline-meta"><span class="timeline-handle">@${escape(p.username)}</span><span class="timeline-date"> · ${escape(date(p.created_at,true))}</span></span></div>${p.repost_id?'<span class="badge neutral">转推</span>':p.reply_to?'<span class="badge neutral">回复</span>':''}</div><div class="post-main"><div class="post-text">${escape(p.text)}</div>${p.text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(p)}${quoteHTML(p)}</div></div></div><footer class="post-footer"><a href="https://x.com/i/status/${encodeURIComponent(p.id)}" target="_blank" rel="noopener noreferrer">查看原推文${icon('arrow')}</a></footer></article>`;}
const timelineObserver='IntersectionObserver' in window?new IntersectionObserver(entries=>{
 if(entries.some(e=>e.isIntersecting)&&!state.postsError)loadMorePosts();
},{rootMargin:'0px 0px 320px 0px'}):null;
function updateTimelineLoader(){
 const count=state.postIDs.size;
 $('posts').setAttribute('aria-busy',String(state.postsLoading));
 $('load-status').textContent=state.postsLoading?(count?'正在加载更多…':'正在加载动态…'):state.postsError?'加载失败，已显示的内容已保留。':count?`已显示 ${number(count)} 条，共 ${number(state.total)} 条${state.hasMore?'':' · 已全部加载'}`:'暂无归档内容';
 $('load-more').hidden=state.postsLoading||(!state.hasMore&&!state.postsError);
 $('load-more').textContent=state.postsError?'重试加载':'加载更多';
}
async function refreshPosts(){
 clearTimeout(searchTimer);timelineObserver?.disconnect();state.postRequest++;
 state.offset=0;state.total=0;state.postIDs.clear();state.postsLoading=false;state.postsError=false;state.hasMore=true;
 $('posts').replaceChildren();$('post-empty').hidden=true;$('result-count').textContent='…';
 await loadMorePosts();
}
async function loadMorePosts(){
 if(!state.authenticated||state.view!=='user'||state.postsLoading||!state.hasMore)return;
 const id=state.postRequest;
 state.postsLoading=true;state.postsError=false;timelineObserver?.disconnect();updateTimelineLoader();
 const q=new URLSearchParams({q:$('search').value,target:state.target,kind:$('kind-filter').value,section:state.timelineTab,offset:String(state.offset)});
 try{
  const r=await api('/posts?'+q);if(id!==state.postRequest||!state.authenticated||state.view!=='user')return;
  state.total=r.total;$('result-count').textContent=number(r.total);
  const fresh=r.items.filter(p=>{if(state.postIDs.has(p.id))return false;state.postIDs.add(p.id);return true;});
  $('posts').insertAdjacentHTML('beforeend',fresh.map(postHTML).join(''));
  state.offset+=r.items.length;state.hasMore=r.items.length>0&&state.offset<r.total;
  $('post-empty').hidden=state.postIDs.size>0;
  const filtered=$('search').value||$('kind-filter').value||state.timelineTab==='media';
  $('post-empty').querySelector('h3').textContent=filtered?'没有找到匹配的动态':state.timelineTab==='replies'?'还没有保存的回复':'还没有保存的推文';
  $('empty-text').textContent=filtered?'搜索也会匹配引用推文；媒体筛选包含引用中的图片和视频。':state.timelineTab==='replies'?'在监控账号设置中开启「包含回复」，采集后会显示该用户发出的回复。':'采集完成后，该用户的推文及引用内容会显示在这里。';
 }catch(err){
  if(id!==state.postRequest||!state.authenticated||state.view!=='user')return;
  state.postsError=true;toast(err.message,true);
 }finally{
  if(id===state.postRequest&&state.authenticated&&state.view==='user'){
   state.postsLoading=false;updateTimelineLoader();
   if(state.hasMore&&!state.postsError)timelineObserver?.observe($('timeline-loader'));
  }
 }
}
$('load-more').addEventListener('click',()=>loadMorePosts());
$('posts').addEventListener('click',event=>{
  const image=event.target.closest('[data-image]');if(image){$('large-image').src=image.dataset.image;$('image-dialog').showModal();}
  if(event.target.classList.contains('expand-text')){const text=event.target.previousElementSibling;const expanded=text.classList.toggle('expanded');event.target.textContent=expanded?'收起全文':'展开全文';}
});
$('image-dialog').addEventListener('close',()=>$('large-image').removeAttribute('src'));
$('search').addEventListener('input',()=>{clearTimeout(searchTimer);state.postRequest++;state.hasMore=false;state.postsLoading=true;timelineObserver?.disconnect();updateTimelineLoader();searchTimer=setTimeout(()=>refreshPosts(),300);});
$('kind-filter').addEventListener('change',()=>refreshPosts());
$('refresh-archive').addEventListener('click',()=>action($('refresh-archive'),async()=>{await refreshOverview();await refreshUsers();}));
$('refresh-user').addEventListener('click',()=>action($('refresh-user'),async()=>{await refreshOverview();await refreshUsers();renderProfile();await refreshPosts();}));


function monitorStatus(m) { if(m.running)return ['采集中','running'];if(m.requested)return ['等待执行','warn'];if(!m.enabled)return ['已暂停','neutral'];if(state.overview?.cookie_blocked)return ['需更新 Cookie','warn'];if(!state.overview?.cookie_ready)return ['等待 Cookie','warn'];if(Number(state.overview?.cooldown_until)>state.overview?.server_time)return ['限流冷却','warn'];if(state.overview?.paused)return ['全局已暂停','neutral'];if(m.last_status==='incomplete')return ['未追赶完成','warn'];if(m.last_error)return ['等待重试','error'];return ['监控中','']; }
async function refreshMonitors() {
  const list=await api('/monitors');if(!state.authenticated)return;state.monitors=list;
  if(!state.settings)state.settings=await api('/settings');
  if(!$('monitor-default-form').contains(document.activeElement))$('default-interval').value=state.settings.default_interval_seconds/60;
  renderMonitors();
}
function renderMonitors(){
  const q=$('monitor-search').value.trim().toLowerCase(),filter=$('monitor-filter').value;
  const list=state.monitors.filter(m=>(m.username+' '+handle(m)).toLowerCase().includes(q)&&(filter==='all'||(filter==='enabled'?m.enabled:!m.enabled)));
  $('monitor-empty').hidden=state.monitors.length>0;$('monitor-no-match').hidden=!state.monitors.length||list.length>0;
  $('monitor-count').textContent=list.length+' / '+state.monitors.length+' 个账号';
  $('monitor-rows').innerHTML=list.map(m=>{const [label,cls]=monitorStatus(m);return `<tr><td class="monitor-identity" data-label="账号"><div class="user-cell"><span class="avatar">${escape(handle(m).slice(0,2).toUpperCase())}</span><div><a href="${userURL(m.username)}"><strong>@${escape(handle(m))}</strong></a>${m.replies?'<div class="table-sub">包含回复</div>':''}${m.current_username&&m.current_username!==m.username?`<div class="table-sub">原 @${escape(m.username)} · 归档已延续</div>`:''}</div></div></td><td data-label="统一周期">${number(m.interval_seconds/60)} 分钟</td><td class="monitor-status" data-label="运行状态"><span class="badge ${cls}">${label}</span>${m.last_error?`<div class="table-sub ${m.last_status==='incomplete'?'':'error'}">${escape(m.last_error)}</div>`:''}</td><td data-label="最近 / 下次批次"><div>${escape(date(m.last_finished,true))}</div><div class="table-sub">${m.enabled?'计划 '+escape(date(m.next_run,true)):'自动轮询已暂停'}</div></td><td data-label="已归档"><strong>${number(m.posts)}</strong><div class="table-sub">上轮新增 ${m.new_posts} 条</div></td><td class="monitor-actions" data-label="操作"><div class="row-actions"><button class="text-button" data-monitor="${m.username}" data-op="${m.running||m.requested?'stop':'run'}">${m.running?'停止':m.requested?'取消排队':'立即采集'}</button><button class="text-button" data-monitor="${m.username}" data-op="edit">编辑</button><button class="text-button" data-monitor="${m.username}" data-op="toggle">${m.enabled?'暂停':'启用'}</button><button class="text-button delete" data-monitor="${m.username}" data-op="delete" ${m.running?'disabled':''}>移除</button></div></td></tr>`;}).join('');
}
$('monitor-search').addEventListener('input',renderMonitors);
$('monitor-filter').addEventListener('change',renderMonitors);
$('monitor-default-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{await api('/settings','PUT',{default_interval_seconds:Math.round(Number($('default-interval').value)*60)});state.settings=await api('/settings');toast('统一轮询周期已保存，全部启用账号将在同一批次采集');await refreshMonitors();});});
function monitorPayload(m) { return {username:m.username,replies:m.replies,enabled:m.enabled}; }
async function openMonitor(m=null) {
  if(!state.settings)state.settings=await api('/settings');state.editing=m?.username||null;
  $('monitor-dialog-title').textContent=m?'编辑监控账号':'添加监控账号';$('monitor-username').value=m?handle(m):'';$('monitor-username').disabled=!!m;
  $('monitor-replies').checked=!!m?.replies;$('monitor-enabled').checked=m?m.enabled:true;$('monitor-dialog').showModal();
}
document.querySelectorAll('[data-action="add"]').forEach(b=>b.addEventListener('click',()=>action(b,()=>openMonitor())));
document.querySelectorAll('[data-close]').forEach(b=>b.addEventListener('click',()=>$(b.dataset.close).close()));
$('monitor-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const payload={username:$('monitor-username').value.trim(),replies:$('monitor-replies').checked,enabled:$('monitor-enabled').checked};
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

function selectSettingsTab(name,focus=false){
 for(const button of document.querySelectorAll('[data-settings-tab]')){
  const selected=button.dataset.settingsTab===name;
  button.setAttribute('aria-selected',String(selected));button.tabIndex=selected?0:-1;
  $('settings-'+button.dataset.settingsTab).hidden=!selected;
  if(selected&&focus)button.focus();
 }
}
for(const button of document.querySelectorAll('[data-settings-tab]')){
 button.addEventListener('click',()=>selectSettingsTab(button.dataset.settingsTab));
 button.addEventListener('keydown',e=>{const names=['account','network','storage'];let i=names.indexOf(button.dataset.settingsTab);if(e.key==='ArrowRight')i=(i+1)%3;else if(e.key==='ArrowLeft')i=(i+2)%3;else if(e.key==='Home')i=0;else if(e.key==='End')i=2;else return;e.preventDefault();selectSettingsTab(names[i],true);});
}
async function refreshSettings() {
  const s=await api('/settings');state.settings=s;
  $('database-help').textContent='保存正文、引用关系、媒体索引、监控计划与通知队列。';
  $('database-label').textContent=s.database_driver==='mysql'?'MySQL · '+s.database_name:'SQLite · 本地数据库';
  $('cookie-badge').textContent=s.cookie_blocked?'需更新':s.cookie_ready?'已保存':'未导入';$('cookie-badge').className='badge '+(s.cookie_blocked?'error':s.cookie_ready?'':'neutral');
  $('cookie-status').textContent=s.cookie_blocked|| (s.cookie_ready?`Cookie 已保存${s.cookie_imported?' · '+date(s.cookie_imported)+' 更新':''}。有效性以采集结果为准。`:'支持粘贴 Cookie 请求头，或导入 JSON / Netscape 文件。');
  $('proxy-mode').value=s.proxy_configured?'proxy':'direct';$('proxy-url').value='';$('proxy-label').textContent='当前：'+s.proxy_label;$('proxy-fields').hidden=!s.proxy_configured;$('chrome-mode').textContent=s.chrome_headless?'Headless':'有界面模式';$('default-interval').value=s.default_interval_seconds/60;
  await refreshStorage();
}
$('proxy-mode').addEventListener('change',()=>$('proxy-fields').hidden=$('proxy-mode').value==='direct');
$('cookie-file').addEventListener('change',async()=>{const file=$('cookie-file').files[0];if(!file)return;if(file.size>1024*1024){toast('Cookie 文件不能超过 1 MiB',true);$('cookie-file').value='';return;}try{$('cookie-content').value=await file.text();$('cookie-file-name').textContent=file.name;}catch{toast('无法读取文件',true);}});
$('cookie-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{const content=$('cookie-content').value.trim();if(!content)throw new Error('请粘贴 Cookie 或选择文件');const r=await api('/cookies','POST',{content});$('cookie-content').value='';$('cookie-file').value='';$('cookie-file-name').textContent='或直接粘贴到上方';toast(r.message);await refreshSettings();await refreshOverview();});});
function proxyChange() { if($('proxy-mode').value==='direct')return {proxy:''};const p=$('proxy-url').value.trim();if(p)return {proxy:p};if(!state.settings?.proxy_configured)throw new Error('请输入代理地址');return {}; }
$('settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{await api('/settings','PUT',proxyChange());toast('代理设置已保存，下一个采集任务生效');await refreshSettings();});});
$('test-proxy').addEventListener('click',()=>action($('test-proxy'),async()=>{$('proxy-result').textContent='正在连接 X…';try{const r=await api('/proxy-test','POST',proxyChange());$('proxy-result').textContent=`收到 HTTP ${r.status} · ${r.latency_ms} ms。${r.message}`;}catch(err){$('proxy-result').textContent=err.message;throw err;}}));
$('test-chrome').addEventListener('click',()=>action($('test-chrome'),async()=>{$('chrome-result').classList.remove('error');$('chrome-result').textContent='正在检查 Chrome、Cookie、X 网络和登录状态…';try{const r=await api('/chrome-test','POST');const mark={passed:'✓',failed:'✕',skipped:'○'};$('chrome-result').textContent=r.steps.map(s=>`${mark[s.status]||'·'} ${s.name}：${s.message}`).join('\n')+`\n耗时：${r.latency_ms} ms`;$('chrome-result').classList.toggle('error',!r.ok);}catch(err){$('chrome-result').textContent=err.message;$('chrome-result').classList.add('error');throw err;}}));
const runLabels={running:['采集中','running'],bounded_complete:['已完成',''],incomplete:['未追赶完成','warn'],failed:['采集失败','error'],media_failed:['媒体未完成','warn'],cancelled:['已停止','neutral'],interrupted:['已中断','warn']};
const stopLabels={limit_reached:'已达到系统安全上限，可能尚未追赶完成',known_boundary_reached:'已到达上次归档边界',max_scrolls_reached:'已达到本轮滚动上限',no_new_posts_observed:'本轮未再发现新内容'};
async function refreshRuns() {
  const runs=await api('/runs');$('runs').innerHTML=runs.length?runs.map(r=>{const [label,cls]=runLabels[r.status]||[r.status,'neutral'];return `<article class="run-row"><div class="run-main"><strong>@${escape(r.target)}</strong><p>本轮读取 ${r.posts} 条 · ${escape(stopLabels[r.message]||r.message||'正在读取时间线')}</p><div class="run-time">${escape(date(r.started_at))}${r.finished_at?' → '+escape(date(r.finished_at,true)):''}</div></div><span class="badge ${cls}">${escape(label)}</span></article>`;}).join(''):'<div class="empty-state"><h3>还没有采集记录</h3><p>添加监控账号后，每次任务的状态都会记录在这里。</p></div>';
}
$('refresh-runs').addEventListener('click',()=>action($('refresh-runs'),refreshRuns));
let refreshing=false;
setInterval(async()=>{if(!state.authenticated||document.hidden||refreshing)return;refreshing=true;try{await refreshOverview();if(state.view==='monitors')await refreshMonitors();if(state.view==='activity')await refreshRuns();if(state.view==='notifications')await refreshNotifications(false);}catch(err){$('service-state').textContent='连接暂时中断';}finally{refreshing=false;}},6000);
(async()=>{try{const r=await api('/session');state.csrf=r.csrf;await enter();}catch{showLogin();}})();
