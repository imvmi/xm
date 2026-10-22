'use strict';
let replyContextSession=0,replyContextNext='',replyContextSelected='',replyContextLoading=false,replyContextSeen=new Set(),replyContextOverflow='';
function closeReplyContext(){if($('reply-context-dialog').open)$('reply-context-dialog').close();}
let replyChildrenCursor='',replyChildrenLoading=false,replyChildrenHasMore=true,replyChildrenSeen=new Set();
function replyContextItem(p,child=false){
 const selected=!child&&p.id===replyContextSelected;
 const relation=child?'回复当前推文':p.reply_to?(p.self_reply?'回复自己的推文':'回复推文 #'+p.reply_to):'';
 return `<li class="reply-context-item${selected?' current':''}${child?' reply-child-item':''}" data-context-post="${escape(p.id)}"${selected?' aria-current="true"':''}><div class="reply-preview-author"><strong>${escape(p.name||p.username)}</strong><span>@${escape(p.username)} · ${escape(date(p.created_at))}</span>${selected?'<span class="badge neutral">当前推文</span>':''}</div>${relation?`<div class="self-reply-label">↳ ${escape(relation)}</div>`:''}<p class="reply-context-text">${escape(p.text||'此推文没有文字，可能包含媒体')}</p><div class="reply-preview-actions"><a href="https://x.com/i/status/${encodeURIComponent(p.id)}" target="_blank" rel="noopener noreferrer">在 X 查看这条推文 ↗</a>${child||(!selected&&p.has_replies)?`<button type="button" class="text-button" data-context-open="${escape(p.id)}">${child?'查看上下文与回复':'查看这条推文的回复'}</button>`:''}</div></li>`;
}
async function loadArchivedReplies(){
 if(replyChildrenLoading||!replyChildrenHasMore||!replyContextSelected)return;
 const session=replyContextSession,request=state.postRequest,id=replyContextSelected;
 const active=()=>session===replyContextSession&&request===state.postRequest&&state.authenticated&&$('reply-context-dialog').open;
 replyChildrenLoading=true;$('reply-children-more').disabled=true;$('reply-children-status').textContent='正在读取已归档回复…';
 try{
  const result=await api('/posts/'+encodeURIComponent(id)+'/replies'+(replyChildrenCursor?'?after='+encodeURIComponent(replyChildrenCursor):''));
  if(!active())return;
  const fresh=result.items.filter(p=>!replyChildrenSeen.has(p.id));fresh.forEach(p=>replyChildrenSeen.add(p.id));
  $('reply-children-items').insertAdjacentHTML('beforeend',fresh.map(p=>replyContextItem(p,true)).join(''));
  const previous=replyChildrenCursor;replyChildrenCursor=result.next_cursor||'';
  replyChildrenHasMore=!!result.has_more&&!!replyChildrenCursor&&replyChildrenCursor!==previous&&fresh.length>0;
  $('reply-children-status').textContent=replyChildrenSeen.size?`已显示 ${replyChildrenSeen.size} 条直接回复${replyChildrenHasMore?'，可继续加载。':'。'}`:'系统尚未收录对这条推文的直接回复。';
  $('reply-children-more').hidden=!replyChildrenHasMore;$('reply-children-more').textContent='加载更多回复';
 }catch(err){if(active()){$('reply-children-status').textContent=err.message;$('reply-children-more').hidden=false;$('reply-children-more').textContent='重试读取回复';}}
 finally{if(session===replyContextSession){replyChildrenLoading=false;$('reply-children-more').disabled=false;}}
}
async function loadReplyContext(){
 if(replyContextLoading||!replyContextNext)return;
 const session=replyContextSession,request=state.postRequest,id=replyContextNext,dialog=$('reply-context-dialog');
 const active=()=>session===replyContextSession&&request===state.postRequest&&state.authenticated&&dialog.open;
 replyContextLoading=true;$('reply-context-more').disabled=true;$('reply-context-status').textContent='正在读取已归档上下文…';
 try{
  const result=await api('/posts/'+encodeURIComponent(id)+'/context');if(!active())return;
  const selected=result.items.find(p=>p.id===replyContextSelected);if(selected)$('reply-children-title').textContent='对 @'+selected.username+' 这条推文的已归档回复';
  const fresh=result.items.filter(p=>!replyContextSeen.has(p.id));fresh.forEach(p=>replyContextSeen.add(p.id));
  $('reply-context-items').insertAdjacentHTML('afterbegin',fresh.map(p=>replyContextItem(p)).join(''));
  const cycle=result.next_cursor&&replyContextSeen.has(result.next_cursor);
  replyContextNext=cycle?'':result.next_cursor||'';
  const messages={beginning:'已到这段回复链的起点。',missing:'更早的推文尚未归档，可前往 X 查看。',cycle:'回复关系存在循环，已停止展开。',more:'还有更早的上下文，可继续加载。'};
  $('reply-context-status').textContent=messages[cycle?'cycle':result.end]||'';
  $('reply-context-more').textContent='加载更早的推文';$('reply-context-more').hidden=!replyContextNext;
 }catch(err){if(active()){$('reply-context-status').textContent=err.message;$('reply-context-more').hidden=false;$('reply-context-more').textContent='重试读取';}}
 finally{if(session===replyContextSession){replyContextLoading=false;$('reply-context-more').disabled=false;}}
}
function openReplyContext(id){
 replyContextSession++;replyContextNext=id;replyContextSelected=id;replyContextSeen.clear();replyContextLoading=false;
 $('reply-context-items').replaceChildren();$('reply-context-more').hidden=true;
 replyChildrenCursor='';replyChildrenLoading=false;replyChildrenHasMore=true;replyChildrenSeen.clear();$('reply-children-items').replaceChildren();$('reply-children-more').hidden=true;$('reply-children-title').textContent='对当前推文的已归档回复';
 if(!$('reply-context-dialog').open){replyContextOverflow=document.body.style.overflow;document.body.style.overflow='hidden';$('reply-context-dialog').showModal();}
 $('reply-context-dialog').scrollTop=0;void loadReplyContext();void loadArchivedReplies();
}
$('posts').addEventListener('click',event=>{const button=event.target.closest('[data-reply-context]');if(button)openReplyContext(button.dataset.replyContext);});
$('reply-children-jump').addEventListener('click',()=>{$('reply-children-title').scrollIntoView({block:'start',behavior:'instant'});});
$('reply-children-more').addEventListener('click',()=>void loadArchivedReplies());
$('reply-context-dialog').addEventListener('click',event=>{const button=event.target.closest('[data-context-open]');if(button)openReplyContext(button.dataset.contextOpen);});
$('reply-context-more').addEventListener('click',()=>void loadReplyContext());
$('reply-context-dialog').addEventListener('close',()=>{
 replyContextSession++;replyContextNext='';replyContextSelected='';replyContextLoading=false;replyContextSeen.clear();
 $('reply-context-items').replaceChildren();$('reply-context-status').textContent='';document.body.style.overflow=replyContextOverflow;
 replyChildrenLoading=false;replyChildrenHasMore=false;replyChildrenCursor='';replyChildrenSeen.clear();$('reply-children-items').replaceChildren();$('reply-children-status').textContent='';
});
