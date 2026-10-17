'use strict';
const $ = id => document.getElementById(id);
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const icon = name => `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
const state = {csrf:'', view:'archive', settings:null, monitors:[], overview:null, offset:0, total:0, editing:null, deleting:null, postRequest:0, authenticated:false};
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
  for(const id of ['posts','monitor-rows','runs'])$(id).replaceChildren();
  $('large-image').removeAttribute('src');$('cookie-content').value='';$('proxy-url').value='';
  document.querySelectorAll('dialog[open]').forEach(d=>d.close());
  state.monitors=[];state.settings=null;state.overview=null;
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
const titles={archive:'推文归档',monitors:'监控账号',activity:'采集记录',settings:'连接设置'};
async function navigate() {
  if(!state.authenticated)return;
  const next=location.hash.slice(1);state.view=titles[next]?next:'archive';
  for(const name of Object.keys(titles)){$('view-'+name).hidden=name!==state.view;document.querySelector(`[data-view="${name}"]`).classList.toggle('active',name===state.view);}
  $('breadcrumb-title').textContent=titles[state.view];document.title=`${titles[state.view]} · XM`;
  if(state.view==='archive'){await refreshTargets();await refreshPosts();}
  else if(state.view==='monitors')await refreshMonitors();
  else if(state.view==='activity')await refreshRuns();
  else if(state.view==='settings')await refreshSettings();
}
window.addEventListener('hashchange',()=>navigate().catch(err=>toast(err.message,true)));
async function refreshTargets() {
  const names=await api('/targets');const select=$('target-filter'),chosen=select.value;
  select.replaceChildren(new Option('全部账号',''));for(const n of names)select.add(new Option('@'+n,n));if(names.includes(chosen))select.value=chosen;
}
function mediaHTML(post) {
  const done=post.media.filter(m=>m.status==='done');let html='';
  if(done.length){html+=`<div class="media-grid ${done.length===1?'single':''}">`;for(const m of done){const src=`/media/${encodeURIComponent(post.id)}/${encodeURIComponent(m.id)}`;if(m.kind==='photo')html+=`<button type="button" class="media-button" data-image="${escape(src)}" aria-label="放大 @${escape(post.username)} 的归档图片"><img src="${escape(src)}" loading="lazy" alt="@${escape(post.username)} 的归档图片"></button>`;else html+=`<video controls playsinline preload="metadata" src="${escape(src)}" aria-label="@${escape(post.username)} 的归档视频"></video>`;}html+='</div>';}
  for(const m of post.media.filter(m=>m.status!=='done'))html+=`<div class="media-pending">${icon('image')}<span>${m.kind==='photo'?'图片':'视频'}${m.status==='failed'?'下载失败':'等待下载'}${m.error?' · '+escape(m.error):''}</span></div>`;
  return html;
}
async function refreshPosts() {
  const id=++state.postRequest;const q=new URLSearchParams({q:$('search').value,target:$('target-filter').value,kind:$('kind-filter').value,offset:String(state.offset)});
  const r=await api('/posts?'+q);if(id!==state.postRequest||!state.authenticated)return;
  state.total=r.total;$('result-count').textContent=number(r.total);
  $('posts').innerHTML=r.items.map(p=>{
    const alt=[...p.username].reduce((sum,c)=>sum+c.charCodeAt(0),0)%4;
    return `<article class="post"><div class="post-body"><div class="post-header"><span class="avatar alt${alt}">${escape(p.username.slice(0,2).toUpperCase())}</span><div><div class="post-author">${escape(p.username)}</div><div class="post-userline">@${escape(p.username)} · ${escape(date(p.created_at,true))}</div></div>${p.protected?`<span class="badge neutral">${icon('lock')}锁推</span>`:p.repost_id?'<span class="badge neutral">转推</span>':p.reply_to?'<span class="badge neutral">回复</span>':''}</div><div class="post-text">${escape(p.text)}</div>${p.text.length>260?'<button class="expand-text" type="button">展开全文</button>':''}${mediaHTML(p)}</div><footer class="post-footer"><span>${icon('clock')}${escape(date(p.saved_at,true))} 归档</span><a href="https://x.com/i/status/${encodeURIComponent(p.id)}" target="_blank" rel="noopener noreferrer">查看原推文${icon('arrow')}</a></footer></article>`;
  }).join('');
  $('post-empty').hidden=r.items.length>0;
  const filtered=$('search').value||$('target-filter').value||$('kind-filter').value;
  $('post-empty').querySelector('h3').textContent=filtered?'没有找到匹配的动态':'你的档案，从第一次关注开始';
  $('empty-text').textContent=filtered?'试试其他关键词、账号或媒体类型。':'导入 Cookie，添加想要监控的账号。保存下来的推文、图片和视频将出现在这里。';
  $('post-empty').querySelector('button').hidden=!!filtered;
  $('page-label').textContent=r.total?`第 ${state.offset+1}–${Math.min(state.offset+r.limit,r.total)} 条，共 ${number(r.total)} 条`:'暂无归档内容';
  $('prev-page').disabled=state.offset===0;$('next-page').disabled=state.offset+r.limit>=r.total;
}
$('posts').addEventListener('click',event=>{
  const image=event.target.closest('[data-image]');if(image){$('large-image').src=image.dataset.image;$('image-dialog').showModal();}
  if(event.target.classList.contains('expand-text')){const text=event.target.previousElementSibling;const expanded=text.classList.toggle('expanded');event.target.textContent=expanded?'收起全文':'展开全文';}
});
$('image-dialog').addEventListener('close',()=>$('large-image').removeAttribute('src'));
$('search').addEventListener('input',()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>{state.offset=0;refreshPosts().catch(e=>toast(e.message,true));},300);});
for(const id of ['target-filter','kind-filter'])$(id).addEventListener('change',()=>{state.offset=0;refreshPosts().catch(e=>toast(e.message,true));});
$('refresh-archive').addEventListener('click',()=>action($('refresh-archive'),async()=>{await refreshOverview();await refreshTargets();await refreshPosts();}));
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
setInterval(async()=>{if(!state.authenticated||document.hidden||refreshing)return;refreshing=true;try{await refreshOverview();if(state.view==='monitors')await refreshMonitors();if(state.view==='activity')await refreshRuns();}catch(err){$('service-state').textContent='连接暂时中断';}finally{refreshing=false;}},6000);
(async()=>{try{const r=await api('/session');state.csrf=r.csrf;await enter();}catch{showLogin();}})();
