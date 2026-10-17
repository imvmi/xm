'use strict';
let notificationState={channels:[],deliveries:[]},notificationEditing='',notificationDeleting='';
function notificationKind(){
 const telegram=$('notification-kind').value==='telegram';$('notification-telegram').hidden=!telegram;$('notification-webhook').hidden=telegram;
 $('notification-bot-token').required=telegram&&!notificationEditing;$('notification-chat-id').required=telegram;$('notification-webhook-url').required=!telegram&&!notificationEditing;
}
function editNotification(id=''){
 $('notification-editor').hidden=false;notificationEditing=id;const c=notificationState.channels.find(c=>c.id===id);$('notification-form').reset();
 $('notification-name').value=c?.name||'';$('notification-kind').value=c?.kind||'telegram';$('notification-kind').disabled=!!c;
 $('notification-chat-id').value=c?.chat_id||'';$('notification-thread-id').value=c?.thread_id||'';
 for(const [input,key] of [['enabled','enabled'],['updates','updates'],['alerts','alerts'],['proxy','use_proxy']])$('notification-'+input).checked=c?c[key]:true;
 $('notification-editor-title').textContent=c?'配置通知渠道':'添加通知渠道';$('notification-test').disabled=!c;$('notification-delete').hidden=!c;
 $('notification-clear-bearer-label').hidden=!c?.bearer_configured;
 $('notification-secret-help').textContent=c?'秘密值不回显，留空保留已保存的 Token 和 Webhook 地址。':'Token 和 Webhook 地址保存在数据库中，不回显。';
 $('notification-result').textContent='';notificationKind();
}
async function refreshNotifications(reset=true){
 const data=await api('/notifications');if(!state.authenticated)return;const previous=notificationState;notificationState=data;
 $('notification-error').hidden=!data.worker_error;$('notification-error').textContent=data.worker_error||'';
 if(reset||JSON.stringify(previous.channels)!==JSON.stringify(data.channels))
 $('notification-channels').innerHTML=data.channels.length?data.channels.map(c=>`<button type="button" class="notification-channel" data-channel="${escape(c.id)}"><div><strong>${escape(c.name)}</strong><span class="badge ${c.enabled?'':'neutral'}">${c.enabled?'已启用':'已停用'}</span></div><p>${c.kind==='telegram'?'Telegram':'Webhook'} · ${[c.updates?'新动态':'',c.alerts?'异常与恢复':''].filter(Boolean).join(' / ')||'未选择事件'}</p><small>编辑 →</small></button>`).join(''):'<div class="notification-empty"><h3>还没有通知渠道</h3><p>添加 Telegram 或 Webhook 渠道，接收更新提醒。</p></div>';
 const labels={pending:['等待发送 / 重试','warn'],sent:['已发送',''],failed:['发送失败','error'],cancelled:['已取消','neutral']};
 if(reset||JSON.stringify(previous.deliveries)!==JSON.stringify(data.deliveries))
 $('notification-deliveries').innerHTML=data.deliveries.length?data.deliveries.map(d=>{const [label,cls]=labels[d.status]||[d.status,'neutral'];const c=data.channels.find(c=>c.id===d.channel_id);return `<article class="notification-delivery"><div><strong>${escape(d.event.title)}</strong><p>${escape(c?.name||'已移除的渠道')} · ${escape(date(d.created_at))} · 已尝试 ${d.attempts} 次</p><p>${escape(d.error||d.event.message)}</p>${d.status==='pending'?`<small>计划发送：${escape(date(d.next_attempt))}</small>`:''}</div><div class="notification-delivery-status"><span class="badge ${cls}">${label}</span>${d.status==='failed'?`<button class="button secondary" type="button" data-retry="${d.id}">重试</button>`:''}</div></article>`;}).join(''):'<div class="notification-empty"><h3>尚无发送记录</h3><p>保存渠道后发送一条测试消息，结果会显示在这里。</p></div>';
 if(reset&&!$('notification-editor').hidden)editNotification(data.channels.some(c=>c.id===notificationEditing)?notificationEditing:'');
}
function focusNotificationEditor(){ $('notification-editor').scrollIntoView({behavior:'smooth',block:'start'});$('notification-name').focus({preventScroll:true}); }
$('notification-new').addEventListener('click',()=>{editNotification();focusNotificationEditor();});
$('notification-close').addEventListener('click',()=>{$('notification-editor').hidden=true;$('notification-form').reset();notificationEditing='';$('notification-new').focus();});
$('notification-kind').addEventListener('change',notificationKind);
$('notification-channels').addEventListener('click',e=>{const b=e.target.closest('[data-channel]');if(b){editNotification(b.dataset.channel);focusNotificationEditor();}});
$('notification-form').addEventListener('submit',e=>{e.preventDefault();action(e.submitter,async()=>{
 const p={id:notificationEditing,name:$('notification-name').value,kind:$('notification-kind').value,bot_token:$('notification-bot-token').value,chat_id:$('notification-chat-id').value,thread_id:Number($('notification-thread-id').value||0),webhook_url:$('notification-webhook-url').value,bearer_token:$('notification-bearer').value,clear_bearer:$('notification-clear-bearer').checked};
 for(const [input,key] of [['enabled','enabled'],['updates','updates'],['alerts','alerts'],['proxy','use_proxy']])p[key]=$('notification-'+input).checked;
 const result=await api('/notifications/channels','POST',p);notificationEditing=result.id;await refreshNotifications();toast('通知渠道已保存');
});});
$('notification-test').addEventListener('click',()=>action($('notification-test'),async()=>{const r=await api('/notifications/channels/'+notificationEditing+'/test','POST');$('notification-result').textContent=r.message;toast(r.message);await refreshNotifications(false);}));
$('notification-refresh').addEventListener('click',()=>action($('notification-refresh'),()=>refreshNotifications(false)));
$('notification-deliveries').addEventListener('click',e=>{const b=e.target.closest('[data-retry]');if(b)action(b,async()=>{await api('/notifications/deliveries/'+b.dataset.retry+'/retry','POST');await refreshNotifications(false);});});
$('notification-delete').addEventListener('click',()=>{notificationDeleting=notificationEditing;$('notification-delete-text').textContent='将移除“'+$('notification-name').value+'”。';$('notification-delete-dialog').showModal();});
$('notification-delete-confirm').addEventListener('click',()=>action($('notification-delete-confirm'),async()=>{await api('/notifications/channels/'+notificationDeleting,'DELETE');$('notification-delete-dialog').close();notificationEditing='';$('notification-editor').hidden=true;await refreshNotifications();toast('通知渠道已移除');}));
