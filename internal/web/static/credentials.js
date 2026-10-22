'use strict';
let apiCredentials=[],apiCredentialEditing='',apiCredentialPending=null,apiCredentialRequest=0;
const apiScopeLabels={'accounts:read':'读取账号','accounts:write':'添加和修改账号','accounts:delete':'取消监控','tasks:read':'读取本凭据的任务','tasks:create':'提交及重试任务','tasks:rescan':'重新扫描','tasks:cancel':'取消本凭据的任务','timeline:read':'读取归档时间线','media:read':'读取归档媒体'};
function clearAPISecret(){$('credential-secret').value='';}
function resetAPICredentials(){apiCredentialRequest++;apiCredentials=[];apiCredentialEditing='';apiCredentialPending=null;clearAPISecret();$('credential-list').replaceChildren();$('credential-audit').replaceChildren();$('credential-editor').hidden=true;$('credential-form').reset();}
function credentialScopeFields(){const all=$('credential-all-accounts').checked;$('credential-accounts').disabled=all;$('credential-accounts').required=!all;$('credential-expiry').disabled=$('credential-no-expiry').checked;}
function editAPICredential(id=''){
 const c=apiCredentials.find(c=>c.id===id);apiCredentialEditing=id;$('credential-form').reset();$('credential-editor').hidden=false;
 $('credential-editor-title').textContent=c?'编辑 API 凭据':'创建 API 凭据';$('credential-name').value=c?.name||'';$('credential-description').value=c?.description||'';$('credential-enabled').checked=c?.enabled??true;
 $('credential-all-accounts').checked=!!c?.all_accounts;$('credential-accounts').value=(c?.accounts||[]).join('\n');
 const expiry=c?c.expires_at:Math.floor(Date.now()/1000)+30*86400;$('credential-no-expiry').checked=expiry===0;
 const d=new Date(expiry*1000);$('credential-expiry').value=expiry?new Date(d.getTime()-d.getTimezoneOffset()*60000).toISOString().slice(0,16):'';
 for(const [id,key,defaultValue] of [['credential-request-limit','request_limit',60],['credential-task-limit','task_limit',5],['credential-queue-limit','queue_limit',5]])$(id).value=c?.[key]??defaultValue;
 for(const el of document.querySelectorAll('[data-api-scope]'))el.checked=(c?.scopes||['accounts:read','timeline:read','media:read']).includes(el.dataset.apiScope);
 credentialScopeFields();acceptSettingsForm('credential-form');$('credential-editor').scrollIntoView({behavior:'smooth',block:'start'});$('credential-name').focus({preventScroll:true});
}
async function refreshAPICredentials(){
 const request=++apiCredentialRequest;const [data,audit]=await Promise.all([api('/credentials'),api('/credentials/audit')]);if(request!==apiCredentialRequest||!state.authenticated)return;
 apiCredentials=data.items;
 $('credential-list').innerHTML=data.items.length?data.items.map(c=>{const expired=c.expires_at&&c.expires_at<=Date.now()/1000,label=c.revoked_at?'已撤销':!c.enabled?'已停用':expired?'已过期':'有效';return `<article class="credential-card"><div><strong>${escape(c.name)}</strong> <span class="badge neutral">${label}</span><p>${escape(c.description)}</p><small>标识 ${escape(c.id)} · 密钥尾号 ${escape(c.secret_tail)}</small><p>${c.all_accounts?'全部账号':escape(c.accounts.join('、'))}</p><p>${c.scopes.map(s=>escape(apiScopeLabels[s]||s)).join(' · ')}</p><small>有效期：${c.expires_at?escape(date(c.expires_at)):'永不过期'} · 最近使用：${c.last_used_at?escape(date(c.last_used_at)):'尚未使用'}</small></div>${c.revoked_at?'':`<div class="form-actions"><button type="button" class="button secondary" data-credential-edit="${escape(c.id)}">编辑</button><button type="button" class="button secondary" data-credential-action="rotate" data-id="${escape(c.id)}">轮换密钥</button><button type="button" class="button danger" data-credential-action="revoke" data-id="${escape(c.id)}">撤销</button></div>`}</article>`;}).join(''):'<p class="muted">尚无 API 凭据。创建后将密钥交付给需要接入的调用方。</p>';
 $('credential-audit').innerHTML=audit.length?audit.map(e=>`<div class="credential-audit-row"><code>${escape(e.action)}</code><small>${escape(date(e.created_at))} · ${escape(e.credential_id)} · HTTP ${Number(e.status)}</small></div>`).join(''):'<p class="muted">暂无操作记录。</p>';
}
function showAPISecret(secret){$('credential-secret').value=secret;$('credential-secret-status').textContent='完整密钥只在本次显示，关闭后无法再次查看。';$('credential-secret-dialog').showModal();}
$('credential-new').addEventListener('click',()=>editAPICredential());
$('credential-refresh').addEventListener('click',()=>action($('credential-refresh'),refreshAPICredentials));
$('credential-close').addEventListener('click',()=>{$('credential-editor').hidden=true;settingsBaselines.delete('credential-form');updateSettingsDraftNotice();});
$('credential-all-accounts').addEventListener('change',credentialScopeFields);$('credential-no-expiry').addEventListener('change',credentialScopeFields);
$('credential-list').addEventListener('click',e=>{
 const edit=e.target.closest('[data-credential-edit]');if(edit){editAPICredential(edit.dataset.credentialEdit);return;}
 const button=e.target.closest('[data-credential-action]');if(!button)return;apiCredentialPending={id:button.dataset.id,kind:button.dataset.credentialAction};
 const c=apiCredentials.find(c=>c.id===apiCredentialPending.id);$('credential-action-text').textContent=apiCredentialPending.kind==='rotate'?`轮换“${c.name}”的密钥后，旧密钥立即失效。请将新密钥更新到调用方。`:`撤销“${c.name}”后，新请求会被拒绝。已提交任务继续执行；此操作无法撤销。`;
 $('credential-action-error').textContent='';$('credential-action-dialog').showModal();
});
$('credential-form').addEventListener('submit',e=>{e.preventDefault();action(e.submitter,async()=>{
 const input={name:$('credential-name').value,description:$('credential-description').value,enabled:$('credential-enabled').checked,scopes:[...document.querySelectorAll('[data-api-scope]:checked')].map(el=>el.dataset.apiScope),all_accounts:$('credential-all-accounts').checked,accounts:$('credential-accounts').value.split(/[\s,，]+/).filter(Boolean),expires_at:$('credential-no-expiry').checked?0:Math.floor(new Date($('credential-expiry').value).getTime()/1000),request_limit:Number($('credential-request-limit').value),task_limit:Number($('credential-task-limit').value),queue_limit:Number($('credential-queue-limit').value)};
 if(!input.scopes.length)throw Error('请至少选择一项权限');
 const result=await api('/credentials'+(apiCredentialEditing?'/'+apiCredentialEditing:''),apiCredentialEditing?'PATCH':'POST',input);if(!state.authenticated)return;
 $('credential-editor').hidden=true;settingsBaselines.delete('credential-form');updateSettingsDraftNotice();if(result.secret)showAPISecret(result.secret);toast('API 凭据已保存');await refreshAPICredentials();
});});
$('credential-action-confirm').addEventListener('click',()=>action($('credential-action-confirm'),async()=>{
 const p=apiCredentialPending;if(!p)return;
 try{const result=await api('/credentials/'+p.id+(p.kind==='rotate'?'/rotate':''),p.kind==='rotate'?'POST':'DELETE');if(!state.authenticated)return;$('credential-action-dialog').close();if(result.secret)showAPISecret(result.secret);await refreshAPICredentials();}catch(e){$('credential-action-error').textContent=e.message;}
}));
$('credential-secret-dialog').addEventListener('close',clearAPISecret);
$('credential-copy').addEventListener('click',()=>action($('credential-copy'),async()=>{
 try{await navigator.clipboard.writeText($('credential-secret').value);$('credential-secret-status').textContent='已复制，请妥善交付给调用方。';}catch{$('credential-secret').focus();$('credential-secret').select();$('credential-secret-status').textContent='请复制已选中的密钥。';}
}));
