'use strict';
const $ = id => document.getElementById(id);
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const icon = name => `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
const state = {csrf:'', view:'home', settings:null, monitors:[], overview:null, offset:0, cursor:'', total:0, editing:null, deleting:null, postRequest:0, authenticated:false, users:[], target:'', timelineTab:'posts', postsLoading:false, postsError:false, hasMore:false, postIDs:new Set(), navRequest:0};
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
  if(typeof resetAPICredentials==='function')resetAPICredentials();
  releaseTimelineMedia();
  userRequest++;userDirectoryCache=null;userStatsValues.clear();userStatsInFlight.clear();lastUserStatsRefresh=0;
  bookmarkSession++;bookmarkOverrides.clear();bookmarkPending.clear();
  settingsBaselines.clear();$('collection-settings-form').reset();$('archive-display-form').reset();$('settings-form').reset();$('remote-cache-form').reset();updateSettingsDraftNotice();
  state.authenticated=false;state.csrf='';state.postRequest++;document.title='登录 · XM';
  $('app').hidden=true;$('login-view').hidden=false;
  for(const id of ['posts','user-list','profile-header','profile-summary','runs'])$(id).replaceChildren();
  $('notification-form').reset();$('notification-channels').replaceChildren();$('notification-deliveries').replaceChildren();
  $('large-image').removeAttribute('src');$('cookie-content').value='';$('proxy-url').value='';$('cookie-api-form').reset();$('cookie-api-result').textContent='';$('rate-limit-reset-result').textContent='';$('cookie-api-token-help').textContent='';
  document.querySelectorAll('dialog[open]').forEach(d=>d.close());
  $('notification-editor').hidden=true;state.monitors=[];monitorListReady=false;monitorRequest++;accountPending.clear();accountExpanded.clear();accountScroll=0;accountRestoring=false;state.users=[];state.settings=null;state.overview=null;state.navRequest++;
}
async function enter() {
  state.settings=await api('/settings');applyArchiveTheme(state.settings.archive_theme);
  state.authenticated=true;$('password').value='';$('login-error').textContent='';$('login-view').hidden=true;$('app').hidden=false;
  // Directory/feed rendering must not wait for global historical statistics.
  void refreshOverview().catch(err=>{if(state.authenticated)toast(err.message,true);});await navigate();
}
$('login-form').addEventListener('submit',async event=>{
  event.preventDefault();const button=event.submitter;button.disabled=true;$('login-error').textContent='';
  try{const r=await api('/login','POST',{username:$('username').value,password:$('password').value});state.csrf=r.csrf;await enter();}catch(err){$('login-error').textContent=err.message;}finally{button.disabled=false;}
});
$('logout').addEventListener('click',()=>action($('logout'),async()=>{await api('/logout','POST');showLogin();}));

async function refreshOverview(force=false) {
  const session=bookmarkSession,o=await api('/overview'+(force?'?refresh=1':''));if(!state.authenticated||session!==bookmarkSession)return;state.overview=o;const c=o.counts;
  $('stat-posts').textContent=number(c.posts);$('stat-today').textContent=number(c.today);$('stat-monitors').textContent=number(c.monitors);$('stat-active').textContent=number(c.active);$('stat-media').textContent=number(c.media);$('stat-bytes').textContent=bytes(c.media_bytes);
  const cooling=Number(o.cooldown_until)>o.server_time;
  let label=o.running?`正在采集 @${o.running}`:o.cookie_blocked?'登录状态待处理':!o.cookie_ready?'等待导入 Cookie':cooling?'限流冷却中':o.paused?'自动采集已暂停':'服务运行中';
  $('service-state').replaceChildren();const dot=document.createElement('span');dot.className='status-dot';$('service-state').append(dot,document.createTextNode(label));
  renderGlobalCollection(o.paused);
  const banner=$('connection-banner');banner.hidden=true;banner.replaceChildren();
  let warning='';
  if(o.cookie_blocked)warning=`采集已暂停：${o.cookie_blocked}。请重新导入 Cookie。`;
  else if(!o.cookie_ready)warning='还没有可用的 Cookie。先完成设置，监控任务才能开始采集。';
  else if(cooling)warning=`X 请求限流，全部采集已进入冷却，最早于 ${date(Number(o.cooldown_until),true)} 重试；实际恢复以 X 响应为准。`;
  else if(o.scheduler_error)warning=o.scheduler_error;
  if(warning){banner.hidden=false;banner.append(document.createTextNode(warning+' '));const link=document.createElement('a');link.href='#settings';link.textContent='前往设置 →';banner.append(link);}
}
const titles={home:'首页',archive:'账号',user:'用户动态',activity:'任务',settings:'设置',notifications:'通知设置'};
function avatar(username, extra='', source='') { const initials=escape(username.slice(0,2).toUpperCase()),alt=[...username].reduce((sum,c)=>sum+c.charCodeAt(0),0)%4;return `<span class="avatar alt${alt} ${extra}"><span class="avatar-fallback">${initials}</span>${source?`<img class="avatar-image" src="${escape(source)}" alt="" loading="lazy">`:''}</span>`; }
document.addEventListener('error',event=>{if(event.target instanceof HTMLImageElement&&event.target.classList.contains('avatar-image'))event.target.remove();},true);
function handle(u){return u.current_username||u.username;}
function userAvatar(username){
 const key=String(username??'').toLowerCase();
 const user=state.users?.find(u=>[u.current_username,u.username].some(name=>String(name??'').toLowerCase()===key));
 return user?.avatar_url||'';
}
function userURL(username) { return '#user/'+encodeURIComponent(username); }
async function navigate() {
  if(!state.authenticated)return;
  releaseTimelineMedia();
  const request=++state.navRequest;state.postRequest++;state.hasMore=false;state.postsLoading=false;clearTimeout(searchTimer);timelineObserver?.disconnect();
  const directoryScroll=accountScroll;
  accountRestoring=false;
  let next=location.hash.slice(1);const credentialsTab=next==='settings/credentials';if(credentialsTab)next='settings';
  if(typeof clearAPISecret==='function'&&next!=='settings')clearAPISecret();
  if(['accounts','archive','monitors'].includes(next)){next='archive';if(location.hash!=='#accounts')history.replaceState(null,'','#accounts');}
  const match=/^user\/([A-Za-z0-9_]{1,15})$/.exec(next);
  state.view=match?'user':titles[next]&&next!=='user'?next:'home';
  accountRestoring=state.view==='archive';
  for(const name of Object.keys(titles))$('view-'+name).hidden=name!==state.view;
  for(const a of document.querySelectorAll('[data-view]')){const active=a.dataset.view===(state.view==='user'?'archive':state.view);a.classList.toggle('active',active);if(active)a.setAttribute('aria-current','page');else a.removeAttribute('aria-current');}window.scrollTo({top:0,behavior:'instant'});
  $('breadcrumb-title').textContent=titles[state.view];document.title=`${titles[state.view]} · XM`;
  if(state.view==='archive'){
    void refreshMonitors().catch(()=>{});
    try{await refreshUsers();}
    finally{
      // Let the directory's responsive layout settle before restoring position.
      if(request===state.navRequest)await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
      if(request===state.navRequest){window.scrollTo({top:directoryScroll,behavior:'instant'});accountRestoring=false;}
    }
  }
  else if(state.view==='home') {
    state.target='';state.timelineTab='';$('search').value='';$('kind-filter').value='';
    $('home-feed').append($('timeline-panel'));syncTabs();
    $('kind-filter').querySelector('option[value="text"]').disabled=false;
    await refreshPosts();
  }
  else if(state.view==='user') {
    document.querySelector('#view-user .profile-feed').append($('timeline-panel'));$('timeline-panel').setAttribute('role','tabpanel');$('timeline-panel').removeAttribute('aria-label');
    state.target=match[1].toLowerCase();state.offset=0;state.timelineTab='posts';$('search').value='';$('kind-filter').value='';
    $('posts').replaceChildren();$('profile-header').replaceChildren();$('profile-top-name').textContent='@'+state.target;$('profile-top-count').textContent='正在读取档案…';
    await refreshUsers(state.target);if(request!==state.navRequest)return;renderProfile();syncTabs();await refreshPosts();
  }
  else if(state.view==='activity')await refreshRuns();
  else if(state.view==='settings'){await refreshSettings();if(credentialsTab)selectSettingsTab('credentials');}
  else if(state.view==='notifications')await refreshNotifications();
}
document.querySelector('.skip-link').addEventListener('click',event=>{event.preventDefault();(state.authenticated?$('main-content'):$('username')).focus();});
window.addEventListener('hashchange',()=>navigate().catch(err=>toast(err.message,true)));
let userDirectoryCache=null,userRequest=0,lastUserStatsRefresh=0;
const userStatsValues=new Map();
const userStatsInFlight=new Set();
function userStat(u,key){return u.stats_pending?'—':number(u[key]);}
function withUserStats(u){const s=userStatsValues.get(u.username);return s?{...u,posts:s.posts,media:s.media,last_saved:s.last_saved,stats_pending:false}:u;}
async function refreshUsers(target='',force=false){
 const request=++userRequest,nav=state.navRequest;
 const current=()=>state.authenticated&&request===userRequest&&nav===state.navRequest;
 const cached=target?(userDirectoryCache||state.users).filter(u=>u.username===target):userDirectoryCache;
 state.users=(cached||[]).map(withUserStats);
 if(state.view==='archive'){
  renderUsers();$('user-empty').hidden=true;$('user-status').hidden=false;
  $('user-status').textContent=cached?'正在更新账号列表…':'正在读取账号列表…';$('user-list').setAttribute('aria-busy','true');
 }else if(cached?.length)renderProfile();
 try{
  const users=await api('/users?stats=0'+(target?'&target='+encodeURIComponent(target):''));if(!current())return;
  if(!target)userDirectoryCache=users;
  state.users=users.map(withUserStats);
  if(state.view==='archive'){renderUsers();$('user-list').setAttribute('aria-busy','false');}
  // Start statistics separately so account timelines can load immediately too.
  void refreshUserStats(target,force,request,nav);
 }catch(err){
  if(!current())return;
  if(state.view==='archive'){$('user-list').setAttribute('aria-busy','false');$('user-status').hidden=false;$('user-status').textContent='账号列表加载失败，请点击刷新重试。';}
  throw err;
 }
}
async function refreshUserStats(target='',force=false,request=userRequest,nav=state.navRequest){
 const current=()=>state.authenticated&&request===userRequest&&nav===state.navRequest;
 if(!current())return;
 const key=nav+':'+request+':'+target;if(userStatsInFlight.has(key))return;userStatsInFlight.add(key);
 if(state.view==='archive'){$('user-status').hidden=false;$('user-status').textContent='正在更新统计…';}
 try{
  const query=new URLSearchParams();if(target)query.set('target',target);if(force)query.set('refresh','1');
  const stats=await api('/users/stats'+(query.size?'?'+query:''));if(!current())return;
  for(const s of stats)userStatsValues.set(s.username,s);
  state.users=state.users.map(withUserStats);lastUserStatsRefresh=Date.now();
  if(state.view==='archive'){
   renderUsers();$('user-status').hidden=!state.users.some(u=>u.stats_pending);$('user-status').textContent='部分统计尚未更新，请稍后刷新。';
  }else if(state.view==='user')renderProfile();
 }catch(err){
  if(!current())return;
  lastUserStatsRefresh=Date.now();
  if(state.view==='archive'){$('user-status').hidden=false;$('user-status').textContent='统计更新失败，已有数据保留，请点击刷新重试。';}
  toast(err.message,true);
 }finally{userStatsInFlight.delete(key);}
}
function renderUsers(){renderAccounts();}
$('user-search').addEventListener('input',renderUsers);
function renderProfile(){
 const u=state.users.find(u=>u.username===state.target)||{username:state.target,name:state.target,posts:0,media:0};
 $('profile-top-name').textContent=u.name;$('profile-top-count').textContent=userStat(u,'posts')+' 条已归档推文';document.title=u.name+' (@'+handle(u)+') · XM';
 $('profile-header').innerHTML=`<div class="profile-cover" aria-hidden="true"></div><div class="profile-identity"><div class="profile-actions">${avatar(handle(u),'profile-avatar',u.avatar_url)}<div class="profile-meta profile-archive-status">${icon('clock')}<span>${u.stats_pending?'正在读取归档统计…':u.last_saved?'最近归档于 '+escape(date(u.last_saved)):'尚未归档'}</span></div></div><h2>${escape(u.name)} ${u.protected?icon('lock'):''}</h2><a href="https://x.com/${encodeURIComponent(handle(u))}" class="user-handle profile-x-link" target="_blank" rel="noopener noreferrer" aria-label="在 X 查看 @${escape(handle(u))}">@${escape(handle(u))}</a>${u.bio?`<p class="profile-bio">${escape(u.bio)}</p>`:''}<div class="profile-numbers"><span><strong>${userStat(u,'posts')}</strong> 条推文</span><span><strong>${userStat(u,'media')}</strong> 个已归档媒体</span><span class="badge neutral">${u.monitored?(u.enabled?'已启用监控':'监控已暂停'):'已归档'}</span></div></div>`;
 $('profile-summary').innerHTML=`<dl><div><dt>已保存推文</dt><dd>${userStat(u,'posts')}</dd></div><div><dt>已归档媒体（含引用）</dt><dd>${userStat(u,'media')}</dd></div><div><dt>统一轮询周期</dt><dd>${u.interval_seconds?number(u.interval_seconds/3600)+' 小时':'未设置'}</dd></div><div><dt>最近采集</dt><dd>${escape(date(u.last_run,true))}</dd></div></dl>`;
}
const timelineTabs=['posts','replies','media','bookmarks'];
const homeTimelineTabs=['','bookmarks'];
function timelineTabID(name,home=false){return home?(name==='bookmarks'?'home-tab-bookmarks':'home-tab-all'):'tab-'+name;}
function syncTabs(){
 const home=state.view==='home';
 for(const name of home?homeTimelineTabs:timelineTabs){const selected=state.timelineTab===name;const tab=$(timelineTabID(name,home));tab.classList.toggle('selected',selected);tab.setAttribute('aria-selected',String(selected));tab.tabIndex=selected?0:-1;}
 $('timeline-panel').setAttribute('role','tabpanel');$('timeline-panel').removeAttribute('aria-label');
 $('timeline-panel').setAttribute('aria-labelledby',timelineTabID(state.timelineTab,home));
 $('home-timeline-description').textContent=state.timelineTab==='bookmarks'?'全部收藏 · 按收藏时间倒序':'已开启首页显示的账号 · 按发布时间倒序';
 $('timeline-order').hidden=state.timelineTab!=='bookmarks';
 $('search').placeholder=state.timelineTab==='bookmarks'?'搜索收藏及引用内容':'搜索推文及引用内容';
 $('kind-filter').querySelector('option[value="text"]').disabled=state.timelineTab==='media';
}
for(const home of [false,true])for(const name of home?homeTimelineTabs:timelineTabs){
 const names=home?homeTimelineTabs:timelineTabs,tab=$(timelineTabID(name,home));
 tab.addEventListener('click',()=>{if(state.timelineTab===name)return;state.timelineTab=name;$('kind-filter').value='';syncTabs();refreshPosts();});
 tab.addEventListener('keydown',event=>{if(event.ctrlKey||event.metaKey||event.altKey)return;let i=names.indexOf(name);if(event.key==='ArrowRight')i=(i+1)%names.length;else if(event.key==='ArrowLeft')i=(i+names.length-1)%names.length;else if(event.key==='Home')i=0;else if(event.key==='End')i=names.length-1;else return;event.preventDefault();const next=$(timelineTabID(names[i],home));next.focus();next.click();});
}
function defaultVideoMuted(){return state.settings?.video_default_muted!==false;}
function mediaHTML(post) {
  const done=post.media.filter(m=>m.status==='done');let html='';
  if(done.length){html+=`<div class="media-grid ${done.length===1?'single':''}">`;for(const m of done){const src=`/media/${encodeURIComponent(post.id)}/${encodeURIComponent(m.id)}`+(m.version?'?v='+encodeURIComponent(m.version):'');if(m.kind==='photo')html+=`<button type="button" class="media-button" data-image="${escape(src)}" aria-label="放大 @${escape(post.username)} 的归档图片"><img src="${escape(src)}" loading="lazy" alt="@${escape(post.username)} 的归档图片"></button>`;else html+=videoHTML(post,src);}html+='</div>';}
  for(const m of post.media.filter(m=>m.status!=='done'))html+=`<div class="media-pending">${icon('image')}<span>${m.kind==='photo'?'图片':'视频'}${m.status==='failed'?'下载失败':'等待下载'}${m.error?' · '+escape(m.error):''}</span></div>`;
  return html;
}
function displayPostText(post){
 let text=String(post.text??'').trimEnd();
 const mediaCount=Array.isArray(post.media)?post.media.length:0;
 for(let i=0;i<mediaCount;i++){
  const trailingMediaURL=/(?:^|\s+)https?:\/\/t\.co\/[A-Za-z0-9]+$/i.exec(text);
  if(!trailingMediaURL)break;
  text=text.slice(0,trailingMediaURL.index).trimEnd();
 }
 return text;
}
let bookmarkRevision=0,bookmarkSession=0;
const bookmarkOverrides=new Map(),bookmarkPending=new Set();
function bookmarkButton(p){
 const label=p.bookmarked?'取消收藏':'收藏推文';
 return `<button type="button" class="bookmark-button${p.bookmarked?' bookmarked':''}" data-bookmark-id="${escape(p.id)}" aria-pressed="${!!p.bookmarked}" aria-label="${label}" title="${label}"${bookmarkPending.has(p.id)?' disabled':''}>${icon('bookmark')}</button>`;
}
function reconcileBookmarks(items,revision){
 for(const p of items){const newer=bookmarkOverrides.get(p.id);if(newer&&newer.revision>revision){p.bookmarked=newer.bookmarked;p.bookmarked_at=newer.bookmarked_at;}if(p.quote)reconcileBookmarks([p.quote],revision);}
}
async function toggleBookmark(button){
 const id=button.dataset.bookmarkId;if(bookmarkPending.has(id))return;
 const session=bookmarkSession,wanted=button.getAttribute('aria-pressed')!=='true';
 bookmarkPending.add(id);
 const buttons=()=>document.querySelectorAll(`[data-bookmark-id="${id}"]`);
 buttons().forEach(el=>{el.disabled=true;});
 try{
  const result=await api('/posts/'+encodeURIComponent(id)+'/bookmark','PUT',{bookmarked:wanted});
  if(session!==bookmarkSession||!state.authenticated)return;
  bookmarkOverrides.set(id,{...result,revision:++bookmarkRevision});
  buttons().forEach(el=>{el.classList.toggle('bookmarked',result.bookmarked);el.setAttribute('aria-pressed',String(result.bookmarked));el.setAttribute('aria-label',result.bookmarked?'取消收藏':'收藏推文');el.title=result.bookmarked?'取消收藏':'收藏推文';});
  if(state.timelineTab==='bookmarks'&&!result.bookmarked){
   const card=$('posts').querySelector(`article[data-post-id="${id}"]`);
   if(card){card.remove();state.postIDs.delete(id);state.total=Math.max(0,state.total-1);state.offset=Math.max(0,state.offset-1);$('result-count').textContent=number(state.total);updateTimelineEmpty();updateTimelineLoader();if(!state.postIDs.size&&state.hasMore)loadMorePosts();}
  }
  toast(result.bookmarked?'已收藏':'已取消收藏');
 }catch(err){if(session===bookmarkSession)toast(err.message,true);}
 finally{if(session===bookmarkSession){bookmarkPending.delete(id);buttons().forEach(el=>{el.disabled=false;});}}
}
function quoteHTML(p,depth=0){
 if(!p.quote_id)return '';
 if(!p.quote||depth>=4)return `<div class="quote-unavailable">${icon('lock')}<span>引用内容暂不可用或尚未归档</span><a href="https://x.com/i/status/${encodeURIComponent(p.quote_id)}" target="_blank" rel="noopener noreferrer">查看原文 ↗</a></div>`;
 const q=p.quote;
 const source=`https://x.com/i/status/${encodeURIComponent(q.id)}`;
 const text=displayPostText(q);
 return `<div class="quote-card"><div class="quote-content"><div class="quote-heading"><div class="quote-author">${avatar(q.username,'tiny-quote',q.avatar_url||userAvatar(q.username))}<strong>${escape(q.name||q.username)}</strong><a class="quote-handle" href="${source}" target="_blank" rel="noopener noreferrer" aria-label="在 X 查看 @${escape(q.username)} 的这条引用推文">@${escape(q.username)}</a><span>· ${escape(date(q.created_at))}</span></div>${bookmarkButton(q)}</div><div class="post-text quote-text">${escape(text)}</div>${text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(q)}${quoteHTML(q,depth+1)}</div></div>`;
}
function postHTML(p){
 const source=`https://x.com/i/status/${encodeURIComponent(p.id)}`,text=displayPostText(p);
 const name=escape(p.name||p.username),author=p.archive_target?`<a href="${userURL(p.archive_target)}" aria-label="查看 @${escape(p.username)} 的归档">${name}</a>`:name;
 return `<article class="post" data-post-id="${escape(p.id)}"><div class="post-body">${avatar(p.username,'post-avatar',p.avatar_url||userAvatar(p.username))}<div class="post-content"><div class="post-header"><div class="timeline-author"><strong>${author}</strong><span class="timeline-meta"><a class="timeline-handle" href="${source}" target="_blank" rel="noopener noreferrer" aria-label="在 X 查看 @${escape(p.username)} 的这条推文">@${escape(p.username)}</a><span class="timeline-date"> · ${escape(date(p.created_at))}</span></span></div>${p.repost_id?'<span class="badge neutral">转推</span>':''}${bookmarkButton(p)}</div><div class="post-main"><div class="post-text">${escape(text)}</div>${text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(p)}${quoteHTML(p)}</div></div></div></article>`;
}
let timelinePrefetchTarget=null;
const timelineObserver='IntersectionObserver' in window?new IntersectionObserver(entries=>{
 if(typeof mediaViewerOpen==='function'&&mediaViewerOpen())return;
 if(entries.some(e=>e.isIntersecting&&(e.target===timelinePrefetchTarget||e.target===$('timeline-loader')))&&!state.postsError)loadMorePosts();
},{rootMargin:'0px 0px 320px 0px'}):null;
function observeTimelineEnd(){
 timelineObserver?.disconnect();timelinePrefetchTarget=null;
 if(typeof mediaViewerOpen==='function'&&mediaViewerOpen())return;
 if(!timelineObserver||state.postsLoading||state.postsError||!state.hasMore||!state.authenticated||!['home','user'].includes(state.view))return;
 // Observe a top-level post, not nested quotes, so tall media gets the same
 // three-post head start as text. Recalculate after append or unbookmark.
 const cards=$('posts').children;
 timelinePrefetchTarget=cards[Math.max(0,cards.length-3)]||$('timeline-loader');
 timelineObserver.observe(timelinePrefetchTarget);
 if(timelinePrefetchTarget!==$('timeline-loader'))timelineObserver.observe($('timeline-loader'));
}
function updateTimelineLoader(){
 const count=state.postIDs.size;
 $('posts').setAttribute('aria-busy',String(state.postsLoading));
 $('load-status').textContent=state.postsLoading?(count?'正在加载更多…':'正在加载动态…'):state.postsError?'加载失败，已显示的内容已保留。':count?`已显示 ${number(count)} 条，共 ${number(state.total)} 条${state.hasMore?'':' · 已全部加载'}`:'暂无归档内容';
 $('load-more').hidden=state.postsLoading||(!state.hasMore&&!state.postsError);
 $('load-more').textContent=state.postsError?'重试加载':'加载更多';
 observeTimelineEnd();
}
function updateTimelineEmpty(){
 $('post-empty').hidden=state.postIDs.size>0;
 const filtered=$('search').value||$('kind-filter').value||state.timelineTab==='media',bookmarks=state.timelineTab==='bookmarks';
 $('post-empty').querySelector('h3').textContent=filtered?'没有找到匹配的动态':bookmarks?'还没有收藏的推文':state.timelineTab==='replies'?'还没有保存的回复':'还没有保存的推文';
 $('empty-text').textContent=filtered?'搜索也会匹配引用推文；媒体筛选包含引用中的图片和视频。':bookmarks?(state.view==='user'?'点击该账号归档中推文或引用右上角的书签，即可在这里查看。':'点击推文右上角的书签，即可保存到收藏。收藏不受账号首页显示开关影响。'):state.timelineTab==='replies'?'在监控账号设置中开启「包含回复」，采集后会显示该用户发出的回复。':state.view==='home'?'暂无可显示的动态。请先采集，或在「账号」中开启「在主时间线显示」。':'采集完成后，该用户的推文及引用内容会显示在这里。';
}
async function refreshPosts(){
 releaseTimelineMedia();
 clearTimeout(searchTimer);timelineObserver?.disconnect();state.postRequest++;
 state.offset=0;state.cursor='';state.total=0;state.postIDs.clear();state.postsLoading=false;state.postsError=false;state.hasMore=true;
 $('posts').replaceChildren();$('post-empty').hidden=true;$('result-count').textContent='…';
 await loadMorePosts();
}
let timelineLoadPromise=null,timelineLoadFlight=null;
function loadMorePosts(options={}){
 if(state.postsLoading){
  if(timelineLoadFlight?.id===state.postRequest){
   if(!options.background)timelineLoadFlight.background=false;
   else if(options.isCurrent)timelineLoadFlight.consumers.push(options.isCurrent);
  }
  return timelineLoadPromise||Promise.resolve();
 }
 const flight={id:state.postRequest,background:!!options.background,consumers:[options.isCurrent||(()=>true)]};
 timelineLoadFlight=flight;timelineLoadPromise=fetchMorePosts(flight);return timelineLoadPromise;
}
async function fetchMorePosts(flight){
 if(!state.authenticated||!['user','home'].includes(state.view)||state.postsLoading||!state.hasMore)return;
 const id=state.postRequest,revision=bookmarkRevision;
 state.postsLoading=true;state.postsError=false;timelineObserver?.disconnect();updateTimelineLoader();
 const q=new URLSearchParams({q:$('search').value,target:state.target,kind:$('kind-filter').value,section:state.timelineTab,offset:String(state.offset)});
 if(state.cursor){q.set('before',state.cursor);q.set('count','0');q.delete('offset');}
 try{
  const r=await api('/posts?'+q);if(flight.background&&!flight.consumers.some(active=>active()))return;if(id!==state.postRequest||!state.authenticated||!['user','home'].includes(state.view))return;
  if(state.timelineTab==='bookmarks'&&revision!==bookmarkRevision){await refreshPosts();return;}
  reconcileBookmarks(r.items,revision);
  if(r.total>=0)state.total=r.total;$('result-count').textContent=number(state.total);
  const fresh=r.items.filter(p=>{if(state.postIDs.has(p.id))return false;state.postIDs.add(p.id);return true;});
  $('posts').insertAdjacentHTML('beforeend',fresh.map(postHTML).join(''));
  state.offset+=r.items.length;state.cursor=r.next_cursor||'';state.hasMore=r.items.length>0&&(r.has_more??state.offset<r.total);
  updateTimelineEmpty();
 }catch(err){
  if(id!==state.postRequest||!state.authenticated||!['user','home'].includes(state.view))return;
  if(flight.background&&!flight.consumers.some(active=>active()))return;
  state.postsError=true;if(!flight.background)toast(err.message,true);
 }finally{
  if(id===state.postRequest&&state.authenticated&&['user','home'].includes(state.view)){
   state.postsLoading=false;updateTimelineLoader();
  }
 }
}
// Media viewers share the timeline's filters, cursor and in-flight request.
async function nextTimelineMedia(current,selector,isCurrent){
 const request=state.postRequest;
 while(request===state.postRequest&&isCurrent()&&current.isConnected){
  const items=[...$('posts').querySelectorAll(selector)],index=items.indexOf(current);
  if(index<0)return null;
  if(items[index+1])return items[index+1];
  if(!state.hasMore)return null;
  const offset=state.offset;
  await loadMorePosts();
  if(state.postsError||state.offset===offset)return null;
 }
 return null;
}
$('load-more').addEventListener('click',()=>loadMorePosts());
$('posts').addEventListener('click',event=>{
  const bookmark=event.target.closest('[data-bookmark-id]');if(bookmark){event.preventDefault();toggleBookmark(bookmark);return;}
  const image=event.target.closest('[data-image]');if(image){
   const items=[...$('posts').querySelectorAll('[data-image]')];
   openGallery(items.map(item=>item.dataset.image),Math.max(0,items.indexOf(image)),true);
  }
  if(event.target.classList.contains('expand-text')){const text=event.target.previousElementSibling;const expanded=text.classList.toggle('expanded');event.target.textContent=expanded?'收起全文':'展开全文';}
});
$('search').addEventListener('input',()=>{clearTimeout(searchTimer);state.postRequest++;state.hasMore=false;state.postsLoading=true;timelineObserver?.disconnect();updateTimelineLoader();searchTimer=setTimeout(()=>refreshPosts(),300);});
$('kind-filter').addEventListener('change',()=>refreshPosts());
$('refresh-archive').addEventListener('click',()=>action($('refresh-archive'),()=>Promise.all([refreshUsers('',true),refreshMonitors(),refreshOverview(true)])));
$('refresh-user').addEventListener('click',()=>action($('refresh-user'),async()=>{const request=state.navRequest;await Promise.all([refreshUsers(state.target,true),refreshOverview(true)]);if(request===state.navRequest){renderProfile();await refreshPosts();}}));
$('refresh-home').addEventListener('click',()=>action($('refresh-home'),async()=>{const request=state.navRequest;await refreshOverview();if(request===state.navRequest)await refreshPosts();}));
for(const header of document.querySelectorAll('[data-timeline-top]')){
 header.addEventListener('click',event=>{
  if(event.target.closest('a,button:not(.timeline-top-button)'))return;
  window.scrollTo({top:0,behavior:window.matchMedia('(prefers-reduced-motion: reduce)').matches?'instant':'smooth'});
 });
}


function monitorStatus(m) { if(m.running)return ['采集中','running'];if(m.requested||m.queued_job)return ['已排队','warn'];if(!m.enabled)return ['已暂停','neutral'];if(state.overview?.cookie_blocked)return ['需更新 Cookie','warn'];if(!state.overview?.cookie_ready)return ['等待 Cookie','warn'];if(Number(state.overview?.cooldown_until)>state.overview?.server_time)return ['限流冷却','warn'];if(state.overview?.paused)return ['全局已暂停','neutral'];if(m.last_status==='budget_wait')return ['等待下次采集','neutral'];if(m.last_status==='incomplete')return ['任务未完成','warn'];if(m.last_error)return ['需关注','error'];return ['监控中','']; }
function checkpointHTML(m) {
  if(!m.catchup)return '';
  const checkpoint=m.checkpoint;
  if(!checkpoint)return '<div class="table-sub checkpoint-status">历史补采中 · 正在建立分页断点</div>';
  const phase=checkpoint.phase==='replies'?'回复时间线':'普通推文时间线';
  const reached=checkpoint.reached_at?`<div class="table-sub checkpoint-reached">已读取至 ${escape(date(checkpoint.reached_at))}<span>（当前最旧内容）</span></div>`:'';
  const saved=checkpoint.saved_at?' · '+escape(date(checkpoint.saved_at,true)):'';
  return `<div class="table-sub checkpoint-status">历史补采中 · ${phase}</div>${reached}<div class="table-sub checkpoint-saved">分页断点已保存，下轮从此处继续${saved}</div>`;
}
const baselineLabels={not_started:'尚未补采历史',backfilling:'历史补采中',visible_history_complete:'当前可见历史已完成',partial:'可见历史范围不确定',blocked:'历史补采受阻'};
function baselineHTML(m){const label=baselineLabels[m.baseline_status]||m.baseline_status||'尚未补采历史';const detail=m.baseline_finished?` · ${date(m.baseline_finished,true)}`:'';return `<div class="table-sub baseline-status">${escape(label+detail)}</div>`;}
function monitorNextStep(m){
 if(m.running)return '<div class="table-sub"><a href="#activity">查看当前进度 →</a></div>';
 if(m.queued_job){
  const j=m.queued_job,o=state.overview||{};
  const reason=o.cookie_blocked?'等待更新 X 登录凭据':!o.cookie_ready?'等待导入 X 登录凭据':Number(o.cooldown_until)>o.server_time?'限流冷却中，最早 '+date(Number(o.cooldown_until),true)+' 重试':o.paused&&j.source!=='manual'?'自动采集已暂停，任务保留在队列':j.retry_at>(o.server_time||0)?'最早 '+date(j.retry_at,true)+' 重试':'等待前序任务完成';
  return `<div class="table-sub">${escape(reason)} · <a href="#activity">查看队列 →</a></div>`;
 }
 if(!['incomplete','media_failed','failed','interrupted'].includes(m.last_status)&&!m.catchup)return '';
 if(!m.enabled||state.overview?.paused)return '<div class="table-sub next-step">自动采集已暂停；可手动继续，或恢复自动采集。</div>';
 if(state.overview?.cookie_blocked)return '<div class="table-sub next-step">请先在设置中更新 X 登录凭据。</div>';
 if(Number(state.overview?.cooldown_until)>state.overview?.server_time)return '<div class="table-sub next-step">限流冷却结束后才能继续，手动任务也需等待。</div>';
 return `<div class="table-sub next-step">${m.next_run>(state.overview?.server_time||0)?'下次计划 '+escape(date(m.next_run,true)):'已到计划时间，等待调度'}；也可手动继续。</div>`;
}
function collectionActions(m){
  if(m.running)return `<button class="text-button" data-monitor="${m.username}" data-op="stop">停止并暂停监控</button>`;
  if(m.queued_job)return `<button class="text-button" data-monitor="${m.username}" data-op="cancel-queue">取消本次排队</button>`;
  if(m.requested)return '<span class="muted">正在加入队列</span>';
  if(m.baseline_status==='not_started'||m.baseline_status==='backfilling'||!m.baseline_status)return `<button class="text-button" data-monitor="${m.username}" data-op="backfill">继续补采历史</button>`;
  const rescan=`<button class="text-button" data-monitor="${m.username}" data-op="rescan">重新扫描历史</button>`;
  return `<button class="text-button" data-monitor="${m.username}" data-op="run">检查新动态</button>${rescan}`;
}
let monitorRequest=0,monitorListReady=false;
async function refreshMonitors() {
 const request=++monitorRequest,nav=state.navRequest;
 try{
  const list=await api('/monitors?stats=0');
  if(!state.authenticated||request!==monitorRequest||nav!==state.navRequest)return;
  state.monitors=list;monitorListReady=true;$('account-monitor-status').hidden=true;
  if(state.view==='archive')renderUsers();
 }catch(err){
  if(state.authenticated&&request===monitorRequest&&nav===state.navRequest){$('account-monitor-status').hidden=false;$('account-monitor-status').textContent='采集状态更新失败；归档仍可浏览，请点击刷新重试。';}
  throw err;
 }
}
function visibleInTimeline(m){return m.show_in_timeline!==false;}
$('monitor-filter').addEventListener('change',renderUsers);
$('monitor-sort').addEventListener('change',renderUsers);
// Drafts stay in the current page only; credentials are never written to browser storage.
const settingsBaselines=new Map();
function settingsSnapshot(id){return JSON.stringify([...$(id).elements].filter(el=>el.id&&!['submit','button','file','fieldset'].includes(el.type)&&el.tagName!=='FIELDSET').map(el=>[el.id,el.type==='checkbox'?el.checked:el.value]));}
function settingsFormDirty(id){return settingsBaselines.has(id)&&settingsBaselines.get(id)!==settingsSnapshot(id);}
function updateSettingsDraftNotice(){
 const dirty=[...settingsBaselines.keys()].some(settingsFormDirty);
 $('settings-draft-notice').hidden=!dirty;
 $('collection-draft-status').textContent=settingsFormDirty('collection-settings-form')?'有未保存修改':'设置已保存';
}
function acceptSettingsForm(id,snapshot=settingsSnapshot(id)){settingsBaselines.set(id,snapshot);updateSettingsDraftNotice();}
function hydrateSettingsForm(id,fill){if(settingsFormDirty(id))return;fill();acceptSettingsForm(id);}
function updateCollectionSummaries(){
 $('plan-summary').textContent=`周期 ${$('default-interval').value} 小时 · 账号间隔 ${$('queue-interval').value} 秒`;
 $('timeline-summary').textContent=`滚动等待 ${$('scroll-delay').value} ± ${$('scroll-jitter').value} 秒`;
 $('browser-summary').textContent=`${$('browser-max-age').value} 小时 · ${$('browser-max-tasks').value} 次任务`;
 $('network-summary').textContent=`请求超时 ${$('request-timeout').value} 秒 · ${{conservative:'保守',balanced:'均衡',fast:'快速'}[$('rate-limit-mode').value]}`;
 $('media-summary').textContent=`单文件 ${$('max-media-mib').value} MiB · 首次重试等待 ${$('media-retry-initial').value} 分钟`;
}
for(const form of document.querySelectorAll('#view-settings form')){
 for(const event of ['input','change'])form.addEventListener(event,()=>{updateSettingsDraftNotice();if(form.id==='collection-settings-form'){updateScrollSpacingHelp();updateCollectionSummaries();}});
 form.addEventListener('invalid',event=>{let el=event.target.parentElement;while(el&&el!==form){if(el.tagName==='DETAILS')el.open=true;el=el.parentElement;}},true);
}
window.addEventListener('beforeunload',event=>{if([...settingsBaselines.keys()].some(settingsFormDirty)){event.preventDefault();event.returnValue='';}});
function updateKeyboardLayout(){document.body.classList.toggle('keyboard-open',!!window.visualViewport&&window.innerHeight-window.visualViewport.height>150);}
window.visualViewport?.addEventListener('resize',updateKeyboardLayout);
const collectionDefaults={timeline:{'scroll-delay':20,'scroll-jitter':5,'timeline-idle-seconds':24,'known-boundary-posts':3},browser:{'browser-max-age':6,'browser-max-tasks':100},network:{'rate-limit-mode':'balanced','request-timeout':60},media:{'max-media-mib':512,'media-retry-initial':5,'media-retry-max':24}};
for(const button of document.querySelectorAll('[data-default-group]'))button.addEventListener('click',()=>{
 for(const [id,value] of Object.entries(collectionDefaults[button.dataset.defaultGroup]))$(id).value=value;
 updateScrollSpacingHelp();updateCollectionSummaries();updateSettingsDraftNotice();
 $('collection-settings-result').textContent='已填入本组默认值，保存后生效。';
});
$('collection-settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const submitted=settingsSnapshot('collection-settings-form');
  const input={queue_interval_seconds:Number($('queue-interval').value),scroll_delay_seconds:Number($('scroll-delay').value),scroll_jitter_seconds:Number($('scroll-jitter').value),timeline_idle_seconds:Number($('timeline-idle-seconds').value),known_boundary_posts:Number($('known-boundary-posts').value),rate_limit_mode:$('rate-limit-mode').value,request_timeout_seconds:Number($('request-timeout').value),max_media_bytes:Number($('max-media-mib').value)*1048576,browser_max_age_seconds:Number($('browser-max-age').value)*3600,browser_max_tasks:Number($('browser-max-tasks').value),media_retry_initial_seconds:Number($('media-retry-initial').value)*60,media_retry_max_seconds:Number($('media-retry-max').value)*3600};
  const interval=Math.round(Number($('default-interval').value)*3600);
  if(interval!==state.settings.default_interval_seconds)input.default_interval_seconds=interval;
  await api('/settings','PUT',input);state.settings=await api('/settings');
  acceptSettingsForm('collection-settings-form',submitted);populateCollectionSettings(state.settings);
  $('collection-settings-result').textContent=input.default_interval_seconds!==undefined?'采集设置已保存；下次自动采集计划已重新安排。其他设置从下一个任务生效，账号间隔会更新当前等待。':'采集设置已保存；从下一个任务生效，账号间隔会更新当前等待。';toast('采集设置已保存');
});});
$('archive-display-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const submitted=settingsSnapshot('archive-display-form');
  await api('/settings','PUT',{archive_theme:$('archive-theme').value,timeline_page_size:Number($('timeline-page-size').value),video_default_muted:$('video-default-muted').checked,video_loop:$('video-loop').checked,media_preload_mode:$('media-preload-mode').value,image_cache_seconds:Number($('image-cache-hours').value)*3600,video_cache_seconds:Number($('video-cache-minutes').value)*60});
  state.settings=await api('/settings');applyArchiveTheme(state.settings.archive_theme);populateArchiveDisplaySettings(state.settings);
  acceptSettingsForm('archive-display-form',submitted);$('archive-display-result').textContent='界面显示设置已保存。';toast('界面显示设置已保存');
});});
function populateArchiveDisplaySettings(s){hydrateSettingsForm('archive-display-form',()=>{$('archive-theme').value=s.archive_theme||'system';$('timeline-page-size').value=s.timeline_page_size||24;$('video-default-muted').checked=s.video_default_muted!==false;$('video-loop').checked=s.video_loop===true;$('media-preload-mode').value=s.media_preload_mode||'standard';$('image-cache-hours').value=(s.image_cache_seconds||604800)/3600;$('video-cache-minutes').value=(s.video_cache_seconds||600)/60;});applyArchiveTheme(s.archive_theme||'system');}
function populateCollectionSettings(s){
 renderGlobalCollection(s.polling_paused);
 hydrateSettingsForm('collection-settings-form',()=>{
  $('default-interval').value=s.default_interval_seconds/3600;$('queue-interval').value=s.queue_interval_seconds;
  $('scroll-delay').value=s.scroll_delay_seconds;$('scroll-jitter').value=s.scroll_jitter_seconds;$('timeline-idle-seconds').value=s.timeline_idle_seconds;$('known-boundary-posts').value=s.known_boundary_posts;updateScrollSpacingHelp();
  $('rate-limit-mode').value=s.rate_limit_mode;$('request-timeout').value=s.request_timeout_seconds;$('max-media-mib').value=s.max_media_bytes/1048576;$('browser-max-age').value=s.browser_max_age_seconds/3600;$('browser-max-tasks').value=s.browser_max_tasks;$('media-retry-initial').value=s.media_retry_initial_seconds/60;$('media-retry-max').value=s.media_retry_max_seconds/3600;
 });updateCollectionSummaries();
}
function updateScrollSpacingHelp(){
  const interval=Number($('scroll-delay').value),jitter=Number($('scroll-jitter').value),help=$('scroll-spacing-help');
  $('scroll-jitter').max=Math.max(0,interval-1);
  if(!Number.isFinite(interval)||!Number.isFinite(jitter)){help.textContent='请输入整数秒数；实际等待不能低于 1 秒。';return;}
  help.textContent=jitter===0?`上一轮页面请求完成后，固定等待 ${interval} 秒再滚动。`:`上一轮页面请求完成后，随机等待 ${interval-jitter}～${interval+jitter} 秒再滚动。`;
}
$('scroll-delay').addEventListener('input',updateScrollSpacingHelp);$('scroll-jitter').addEventListener('input',updateScrollSpacingHelp);
async function openMonitor(m=null,readd=false) {
  if(!state.settings)state.settings=await api('/settings');state.editing=m&&!readd?m.username:null;state.readding=readd?m.username:null;
  $('monitor-dialog-title').textContent=readd?'重新添加监控':m?'编辑监控账号':'添加账号';$('monitor-username').value=m?handle(m):'';$('monitor-username').disabled=!!m;
  $('monitor-replies').checked=!!m?.replies;$('monitor-enabled').checked=m&&!readd?m.enabled:true;$('monitor-timeline').checked=m?visibleInTimeline(m):true;$('monitor-dialog').showModal();
}
document.querySelectorAll('[data-action="add"]').forEach(b=>b.addEventListener('click',()=>action(b,()=>openMonitor())));
document.querySelectorAll('[data-close]').forEach(b=>b.addEventListener('click',()=>$(b.dataset.close).close()));
$('monitor-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{
  const payload={username:state.readding||$('monitor-username').value.trim(),replies:$('monitor-replies').checked,enabled:$('monitor-enabled').checked,show_in_timeline:$('monitor-timeline').checked};
  await api(state.editing?'/monitors/'+encodeURIComponent(state.editing):'/monitors',state.editing?'PUT':'POST',payload);$('monitor-dialog').close();toast('监控账号已保存');await refreshOverview();if(state.view==='archive')await refreshAccountsAfterChange();else{userDirectoryCache=null;location.hash='accounts';}
});});
$('user-list').addEventListener('click',event=>handleAccountAction(event));
$('confirm-delete').addEventListener('click',()=>action($('confirm-delete'),async()=>{
 const target=state.deleting;accountPending.add(target);
 try{await api('/monitors/'+encodeURIComponent(target),'DELETE');$('confirm-dialog').close();toast('监控已取消，归档和首页显示偏好保留');await refreshAccountsAfterChange();}
 finally{accountPending.delete(target);renderUsers();}
}));
let globalCollectionPending=false;
function renderGlobalCollection(paused){
 if(globalCollectionPending)return;
 const button=$('global-auto-collection');button.disabled=false;
 button.setAttribute('aria-checked',String(!paused));
 $('global-collection-state').textContent=paused?'已关闭':'已开启';
}
$('global-auto-collection').addEventListener('click',()=>action($('global-auto-collection'),async()=>{
 const paused=$('global-auto-collection').getAttribute('aria-checked')==='true';
 globalCollectionPending=true;
 try{
  await api('/settings','PUT',{polling_paused:paused});
  if(state.settings)state.settings.polling_paused=paused;
  if(state.overview)state.overview.paused=paused;
  toast(paused?'已暂停自动采集，当前任务继续执行':'已恢复自动采集');
  await refreshOverview();await refreshMonitors();
 }finally{globalCollectionPending=false;renderGlobalCollection(state.overview?.paused??state.settings?.polling_paused??!paused);}
}));

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
 button.addEventListener('keydown',e=>{const names=[...document.querySelectorAll('[data-settings-tab]')].map(b=>b.dataset.settingsTab);let i=names.indexOf(button.dataset.settingsTab);if(e.key==='ArrowRight')i=(i+1)%names.length;else if(e.key==='ArrowLeft')i=(i+names.length-1)%names.length;else if(e.key==='Home')i=0;else if(e.key==='End')i=names.length-1;else return;e.preventDefault();selectSettingsTab(names[i],true);});
}
async function refreshSettings() {
  const s=await api('/settings');state.settings=s;populateArchiveDisplaySettings(s);
  $('database-help').textContent='保存正文、引用关系、媒体索引、监控计划与通知队列。';
  $('database-label').textContent=s.database_driver==='mysql'?'MySQL · '+s.database_name:'SQLite · 本地数据库';
  $('cookie-badge').textContent=s.cookie_blocked?'需更新':s.cookie_ready?'已保存':'未导入';$('cookie-badge').className='badge '+(s.cookie_blocked?'error':s.cookie_ready?'':'neutral');
  $('cookie-status').textContent=s.cookie_blocked|| (s.cookie_ready?`Cookie 已保存${s.cookie_imported?' · '+date(s.cookie_imported)+' 更新':''}。有效性以采集结果为准。`:'支持粘贴 Cookie 请求头，或导入 JSON / Netscape 文件。');
  hydrateSettingsForm('settings-form',()=>{$('proxy-mode').value=s.proxy_configured?'proxy':'direct';$('proxy-url').value='';$('proxy-label').textContent='当前：'+s.proxy_label;$('proxy-fields').hidden=!s.proxy_configured;});$('chrome-mode').textContent=s.chrome_headless?'Headless':'有界面模式';populateCollectionSettings(s);
  if(!settingsBaselines.has('cookie-form'))acceptSettingsForm('cookie-form');
  await refreshCookieSource();
  await refreshStorage();
  if(typeof refreshMigrations==='function')await refreshMigrations();
  if(typeof refreshAPICredentials==='function')await refreshAPICredentials();
}
async function refreshCookieSource() {
  const c=await api('/cookies/source');
  hydrateSettingsForm('cookie-api-form',()=>{$('cookie-api-url').value=c.url||'';$('cookie-api-token').value='';$('cookie-api-clear-token').checked=false;
  $('cookie-api-auto').checked=c.url?c.auto_refresh:true;$('cookie-api-proxy').checked=!!c.use_proxy;});
  $('cookie-api-token-help').textContent=(c.token_configured?'Token 已保存，留空保留。':'无需认证时可留空。')+'修改接口地址后需重新填写 Token。';
  $('cookie-api-result').textContent=c.last_error?'最近获取失败：'+c.last_error:c.last_fetched?'最近成功获取：'+date(c.last_fetched):'';
}
$('cookie-api-form').addEventListener('submit',event=>{
  event.preventDefault();const fetchNow=event.submitter?.value==='fetch',fields=$('cookie-api-fields');
  if(fields.disabled)return;
  const input={url:$('cookie-api-url').value.trim(),token:$('cookie-api-token').value.trim(),clear_token:$('cookie-api-clear-token').checked,auto_refresh:$('cookie-api-auto').checked,use_proxy:$('cookie-api-proxy').checked};
  action(null,async()=>{
    fields.disabled=true;$('cookie-api-result').textContent=fetchNow?'正在保存配置并获取 Cookie…':'正在保存配置…';
    try {
      await api('/cookies/source','PUT',input);$('cookie-api-token').value='';acceptSettingsForm('cookie-api-form');
      const r=fetchNow?await api('/cookies/fetch','POST'): {message:'Cookie API 配置已保存到数据库'};
      await refreshSettings();await refreshOverview();$('cookie-api-result').textContent=r.message;toast(r.message);
    } catch(err) { $('cookie-api-result').textContent=err.message;throw err; }
    finally { fields.disabled=false; }
  });
});
$('proxy-mode').addEventListener('change',()=>$('proxy-fields').hidden=$('proxy-mode').value==='direct');
$('reset-rate-limit').addEventListener('click',()=>action($('reset-rate-limit'),async()=>{
  $('rate-limit-reset-result').textContent='正在清除 XM 采集冷却…';
  try {
    const r=await api('/rate-limit/reset','POST');
    $('rate-limit-reset-result').textContent=r.message;toast(r.message);
    await refreshOverview();
  } catch(err) { $('rate-limit-reset-result').textContent=err.message;throw err; }
}));
$('cookie-file').addEventListener('change',async()=>{const file=$('cookie-file').files[0];if(!file)return;if(file.size>1024*1024){toast('Cookie 文件不能超过 1 MiB',true);$('cookie-file').value='';return;}try{$('cookie-content').value=await file.text();updateSettingsDraftNotice();$('cookie-file-name').textContent=file.name;}catch{toast('无法读取文件',true);}});
$('cookie-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{const content=$('cookie-content').value.trim();if(!content)throw new Error('请粘贴 Cookie 或选择文件');const r=await api('/cookies','POST',{content});$('cookie-content').value='';$('cookie-file').value='';$('cookie-file-name').textContent='或直接粘贴到上方';acceptSettingsForm('cookie-form');toast(r.message);await refreshSettings();await refreshOverview();});});
function proxyChange() { if($('proxy-mode').value==='direct')return {proxy:''};const p=$('proxy-url').value.trim();if(p)return {proxy:p};if(!state.settings?.proxy_configured)throw new Error('请输入代理地址');return {}; }
$('settings-form').addEventListener('submit',event=>{event.preventDefault();action(event.submitter,async()=>{const submitted=settingsSnapshot('settings-form');await api('/settings','PUT',proxyChange());acceptSettingsForm('settings-form',submitted);toast('代理设置已保存，下一个采集任务生效');await refreshSettings();});});
$('test-proxy').addEventListener('click',()=>action($('test-proxy'),async()=>{$('proxy-result').textContent='正在连接 X…';try{const r=await api('/proxy-test','POST',proxyChange());$('proxy-result').textContent=`收到 HTTP ${r.status} · ${r.latency_ms} ms。${r.message}`;}catch(err){$('proxy-result').textContent=err.message;throw err;}}));
$('test-chrome').addEventListener('click',()=>action($('test-chrome'),async()=>{$('chrome-result').classList.remove('error');$('chrome-result').textContent='正在检查 Chrome、Cookie、X 网络和登录状态…';try{const r=await api('/chrome-test','POST');const mark={passed:'✓',failed:'✕',skipped:'○'};$('chrome-result').textContent=r.steps.map(s=>`${mark[s.status]||'·'} ${s.name}：${s.message}`).join('\n')+`\n耗时：${r.latency_ms} ms`;$('chrome-result').classList.toggle('error',!r.ok);}catch(err){$('chrome-result').textContent=err.message;$('chrome-result').classList.add('error');throw err;}}));
const runLabels={running:['采集中','running'],bounded_complete:['已完成',''],incomplete:['本轮未完成','warn'],budget_wait:['历史等待','neutral'],failed:['采集失败','error'],media_failed:['媒体未完成','warn'],cancelled:['已停止','neutral'],interrupted:['已中断','warn']};
const stopLabels={limit_reached:'已达到系统安全上限，可能尚未追赶完成',known_boundary_reached:'已到达上次归档边界',max_scrolls_reached:'已达到本轮滚动上限',no_new_posts_observed:'本轮未再发现新内容'};
const jobLabels={queued:['等待执行','warn'],running:['正在采集','running'],waiting_budget:['等待执行','warn'],waiting_rate:['限流冷却','warn'],waiting_auth:['等待登录恢复','warn'],retrying:['等待重试','warn']};
const sourceLabels={api:'外部 API',manual:'手动',automatic:'自动批次',retry:'自动重试',legacy:'历史任务'};
const modeLabels={incremental:'增量采集',backfill:'历史补采',rescan:'重新扫描'};
const phaseLabels={preparing:'准备任务',timeline:'读取时间线',avatar:'保存头像',media:'下载与归档媒体',finishing:'保存执行结果'};
function durationLabel(seconds){const n=Math.max(0,Math.floor(Number(seconds)||0));return n>=3600?`${Math.floor(n/3600)} 小时 ${Math.floor(n%3600/60)} 分钟`:n>=60?`${Math.floor(n/60)} 分 ${n%60} 秒`:`${n} 秒`;}
function jobHTML(job,current=false){
 const [label,cls]=jobLabels[job.status]||[job.status,'neutral'];
 const mode=modeLabels[job.mode]||job.mode||'增量采集';
 const detail=current?`${mode} · 已读取 ${number(job.posts_read)} 条 · 新增 ${number(job.new_posts)} 条 · 第 ${number(job.attempts)} 次执行`:`${mode} · ${sourceLabels[job.source]||job.source} · ${job.message||'等待前序任务完成'}`;
 const progress=current?`<p class="job-phase">${escape(phaseLabels[job.phase]||'等待进度更新')}${job.remaining_seconds!=null?' · 已运行 '+escape(durationLabel(job.elapsed_seconds))+' · '+(job.remaining_seconds>0?'时限剩余 '+escape(durationLabel(job.remaining_seconds)):'时限已到，正在收尾'):''}</p>${job.media_total!=null?`<p>此账号媒体已归档 ${number(job.media_done)} / ${number(job.media_total)} 个（含引用媒体）</p>`:''}`:'';
 const controls=current?`<button class="text-button delete" type="button" data-stop-target="${escape(job.target)}">停止并暂停监控</button>`:`<button class="text-button delete" type="button" data-cancel-job="${Number(job.id)}">取消本次排队</button>`;
 return `<article class="job-row ${current?'current':''}"><div class="job-position">${current?icon('clock'):number(job.position)}</div><div class="run-main"><strong>@${escape(job.target)}</strong><p>${escape(detail)}</p>${progress}<div class="run-time">${current?'开始于 '+escape(date(job.started_at,true)):'入队于 '+escape(date(job.queued_at,true))}${!current&&job.retry_at?' · 最早 '+escape(date(job.retry_at,true))+' 重试':''}</div></div><div class="job-actions"><span class="badge ${cls}">${escape(label)}</span>${controls}</div></article>`;
}
async function refreshRuns() {
  const payload=await api('/activity'),legacy=Array.isArray(payload),runs=legacy?payload:payload.runs;
  if(!legacy){const current=payload.current,queue=payload.queue||[];$('current-job-count').textContent=current?'1':'0';$('queue-count').textContent=number(queue.length);$('current-job').innerHTML=current?jobHTML(current,true):'<div class="activity-empty">当前没有正在执行的任务</div>';$('job-queue').innerHTML=queue.length?queue.map(j=>jobHTML(j)).join(''):'<div class="activity-empty">等待队列为空</div>';const wait=$('queue-wait');wait.hidden=!payload.wait_reason;wait.textContent=payload.wait_reason+(payload.wait_until?' · 预计 '+date(payload.wait_until,true)+' 后继续':'');}
  $('runs').innerHTML=runs.length?runs.map(r=>{const [label,cls]=runLabels[r.status]||[r.status,'neutral'];const source=r.source&&r.source!=='legacy'?` · ${sourceLabels[r.source]||r.source}`:'';const mode=` · ${modeLabels[r.mode]||r.mode||'增量采集'}`;return `<article class="run-row"><div class="run-main"><strong>@${escape(r.target)}</strong><p>本轮读取 ${r.posts} 条 · ${r.new_posts==null?'新增未记录':`新增 ${r.new_posts} 条`} · ${escape(stopLabels[r.message]||r.message||'正在读取时间线')}</p><div class="run-time">${escape(date(r.started_at))}${r.finished_at?' → '+escape(date(r.finished_at,true)):''}${escape(mode+source)}</div>${['incomplete','media_failed','failed','interrupted'].includes(r.status)?'<p><a href="#accounts">查看计划或继续采集 →</a></p>':''}</div><span class="badge ${cls}">${escape(label)}</span></article>`;}).join(''):'<div class="empty-state"><h3>还没有已结束的执行</h3><p>当前任务结束后，执行结果会显示在这里。</p></div>';
}
$('refresh-runs').addEventListener('click',()=>action($('refresh-runs'),refreshRuns));
$('current-job').addEventListener('click',event=>{const button=event.target.closest('[data-stop-target]');if(!button)return;action(button,async()=>{await api('/monitors/'+encodeURIComponent(button.dataset.stopTarget)+'/stop','POST');toast('已请求停止并暂停此账号；已保存内容保留');await refreshRuns();await refreshOverview();});});
$('job-queue').addEventListener('click',event=>{const button=event.target.closest('[data-cancel-job]');if(!button)return;action(button,async()=>{const result=await api('/activity/jobs/'+button.dataset.cancelJob,'DELETE');toast(result.message);await refreshRuns();if(state.view==='archive')await refreshMonitors();});});
let refreshing=false;
setInterval(async()=>{if(!state.authenticated||document.hidden||refreshing)return;refreshing=true;try{await refreshOverview();if(state.view==='archive')await refreshMonitors();if(state.view==='activity')await refreshRuns();if(state.view==='notifications')await refreshNotifications(false);if(['archive','user'].includes(state.view)&&Date.now()-lastUserStatsRefresh>=30000){if(state.view==='archive'){if(!userStatsInFlight.size)await refreshUsers();}else await refreshUserStats(state.target);}}catch(err){$('service-state').textContent='连接暂时中断';}finally{refreshing=false;}},6000);
document.addEventListener('DOMContentLoaded',async()=>{try{const r=await api('/session');state.csrf=r.csrf;await enter();}catch{showLogin();}});
