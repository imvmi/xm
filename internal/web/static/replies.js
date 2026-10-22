'use strict';
let replyContextSession=0,replyContextNext='',replyContextSelected='',replyContextLoading=false,replyContextSeen=new Set(),replyContextOverflow='';
function closeReplyContext(){if($('reply-context-dialog').open)$('reply-context-dialog').close();}
function replyContextItem(p){
 const selected=p.id===replyContextSelected;
 return `<li class="reply-context-item${selected?' current':''}" data-context-post="${escape(p.id)}"${selected?' aria-current="true"':''}><div class="reply-preview-author"><strong>${escape(p.name||p.username)}</strong><span>@${escape(p.username)} · ${escape(date(p.created_at))}</span>${selected?'<span class="badge neutral">当前推文</span>':''}</div>${p.self_reply?'<div class="self-reply-label">↳ 回复自己的推文</div>':''}<p class="reply-context-text">${escape(p.text||'此推文没有文字，可能包含媒体')}</p><a href="https://x.com/i/status/${encodeURIComponent(p.id)}" target="_blank" rel="noopener noreferrer">在 X 查看这条推文 ↗</a></li>`;
}
async function loadReplyContext(){
 if(replyContextLoading||!replyContextNext)return;
 const session=replyContextSession,request=state.postRequest,id=replyContextNext,dialog=$('reply-context-dialog');
 const active=()=>session===replyContextSession&&request===state.postRequest&&state.authenticated&&dialog.open;
 replyContextLoading=true;$('reply-context-more').disabled=true;$('reply-context-status').textContent='正在读取已归档上下文…';
 try{
  const result=await api('/posts/'+encodeURIComponent(id)+'/context');if(!active())return;
  const fresh=result.items.filter(p=>!replyContextSeen.has(p.id));fresh.forEach(p=>replyContextSeen.add(p.id));
  $('reply-context-items').insertAdjacentHTML('afterbegin',fresh.map(replyContextItem).join(''));
  const cycle=result.next_cursor&&replyContextSeen.has(result.next_cursor);
  replyContextNext=cycle?'':result.next_cursor||'';
  const messages={beginning:'已到这段回复链的起点。',missing:'更早的推文尚未归档，可前往 X 查看。',other_author:'前一条属于其他作者，此处仅展示同账号上下文。',other_reply:'已到这段自回复链的起点；更早内容是对其他账号的回复。',cycle:'回复关系存在循环，已停止展开。',more:'还有更早的上下文，可继续加载。'};
  $('reply-context-status').textContent=messages[cycle?'cycle':result.end]||'';
  $('reply-context-more').textContent='加载更早的推文';$('reply-context-more').hidden=!replyContextNext;
 }catch(err){if(active()){$('reply-context-status').textContent=err.message;$('reply-context-more').hidden=false;$('reply-context-more').textContent='重试读取';}}
 finally{if(session===replyContextSession){replyContextLoading=false;$('reply-context-more').disabled=false;}}
}
function openReplyContext(id){
 if($('reply-context-dialog').open)return;
 replyContextSession++;replyContextNext=id;replyContextSelected=id;replyContextSeen.clear();replyContextLoading=false;
 $('reply-context-items').replaceChildren();$('reply-context-more').hidden=true;
 replyContextOverflow=document.body.style.overflow;document.body.style.overflow='hidden';
 $('reply-context-dialog').showModal();void loadReplyContext();
}
$('posts').addEventListener('click',event=>{const button=event.target.closest('[data-reply-context]');if(button)openReplyContext(button.dataset.replyContext);});
$('reply-context-more').addEventListener('click',()=>void loadReplyContext());
$('reply-context-dialog').addEventListener('close',()=>{
 replyContextSession++;replyContextNext='';replyContextSelected='';replyContextLoading=false;replyContextSeen.clear();
 $('reply-context-items').replaceChildren();$('reply-context-status').textContent='';document.body.style.overflow=replyContextOverflow;
});
