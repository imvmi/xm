'use strict';
let postThreadSession=0,threadContextNext='',threadReplyCursor='',threadRepliesMore=false;
let threadContextSeen=new Set(),threadReplySeen=new Set(),threadContextLoading=false,threadRepliesLoading=false;
const postReturnStack=[];
let pendingPostReturn=null,detailSwipe=null,detailSwipeTimer=null,detailSwipeSuppressUntil=0;

function routeHash(){return location.hash||'#home';}
function openPostDetail(id){
 if(!/^\d{1,64}$/.test(String(id))||String(id)===state.focusPost)return;
 postReturnStack.push({hash:routeHash(),scroll:scrollY});
 location.hash='post/'+encodeURIComponent(id);
}
function leavePostDetail(){
 const previous=postReturnStack.pop();
 if(!previous){location.hash='home';return;}
 pendingPostReturn=previous;
 history.back();
}
async function restorePostReturn(){
 if(!pendingPostReturn||routeHash()!==pendingPostReturn.hash)return;
 const target=pendingPostReturn;pendingPostReturn=null;
 await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
 window.scrollTo({top:target.scroll,behavior:'instant'});
}
function resetDetailSwipe(){
 clearTimeout(detailSwipeTimer);detailSwipeTimer=null;detailSwipe=null;
 const feed=$('home-feed');feed.classList.remove('swiping','swipe-returning');feed.style.removeProperty('transform');feed.style.removeProperty('opacity');
}
function resetPostThread(){
 postThreadSession++;threadContextNext='';threadReplyCursor='';threadRepliesMore=false;
 threadContextSeen.clear();threadReplySeen.clear();threadContextLoading=false;threadRepliesLoading=false;
 resetDetailSwipe();
}
function activeThread(session,request,id){
 return session===postThreadSession&&request===state.navRequest&&state.focusPost===id&&state.authenticated;
}
function addThreadPosts(items,role,position){
 const fresh=items.filter(p=>p.id!==state.focusPost&&!threadContextSeen.has(p.id)&&!threadReplySeen.has(p.id));
 for(const p of fresh){(role==='ancestor'?threadContextSeen:threadReplySeen).add(p.id);state.postIDs.add(p.id);}
 if(!fresh.length)return 0;
 const html=fresh.map(p=>postHTML(p,{threadRole:role})).join('');
 if(position==='prepend')$('posts').insertAdjacentHTML('afterbegin',html);
 else $('thread-replies-status').insertAdjacentHTML('beforebegin',html);
 if(typeof observeTimelineVideos==='function')observeTimelineVideos();
 return fresh.length;
}
async function loadThreadContext(){
 if(threadContextLoading||!threadContextNext)return;
 const session=postThreadSession,request=state.navRequest,selected=state.focusPost,id=threadContextNext;
 threadContextLoading=true;
 const more=$('thread-context-more');if(more){more.disabled=true;more.textContent='正在加载…';}
 try{
  const result=await api('/posts/'+encodeURIComponent(id)+'/context');if(!activeThread(session,request,selected))return;
  addThreadPosts(result.items,'ancestor','prepend');
  const next=result.next_cursor||'';threadContextNext=next&&next!==id?next:'';
  const contextMessage=result.end==='missing'?'更早的推文尚未归档。':result.end==='cycle'?'回复关系存在循环，已停止展开。':'';
  if(contextMessage&&!$('thread-context-status'))$('posts').insertAdjacentHTML('afterbegin',`<p id="thread-context-status" class="thread-status">${contextMessage}</p>`);
  let button=$('thread-context-more');
  if(threadContextNext){
   if(!button){$('posts').insertAdjacentHTML('afterbegin','<button id="thread-context-more" class="thread-load-more" type="button">加载更早的推文</button>');button=$('thread-context-more');}
   $('posts').prepend(button);
   button.hidden=false;button.disabled=false;button.textContent='加载更早的推文';
  }else if(button)button.remove();
 }catch(err){
  if(isRequestAbort(err))return;
  if(activeThread(session,request,selected)){
   let button=$('thread-context-more');
   if(!button){$('posts').insertAdjacentHTML('afterbegin','<button id="thread-context-more" class="thread-load-more" type="button"></button>');button=$('thread-context-more');}
   $('posts').prepend(button);
   button.hidden=false;button.disabled=false;button.textContent='重试加载更早的推文';toast(err.message,true);
  }
 }finally{if(session===postThreadSession)threadContextLoading=false;}
}
async function loadThreadReplies(){
 if(threadRepliesLoading||(!threadRepliesMore&&threadReplyCursor))return;
 const session=postThreadSession,request=state.navRequest,id=state.focusPost;
 threadRepliesLoading=true;
 const more=$('thread-replies-more');if(more){more.disabled=true;more.textContent='正在加载…';}
 $('thread-replies-status').textContent=threadReplySeen.size?'正在加载更多回复…':'正在读取已归档回复…';
 try{
  const suffix=threadReplyCursor?'?after='+encodeURIComponent(threadReplyCursor):'';
  const result=await api('/posts/'+encodeURIComponent(id)+'/replies'+suffix);if(!activeThread(session,request,id))return;
  addThreadPosts(result.items,'reply','append');
  const previous=threadReplyCursor;threadReplyCursor=result.next_cursor||'';
  threadRepliesMore=!!result.has_more&&!!threadReplyCursor&&threadReplyCursor!==previous;
  $('thread-replies-status').textContent=threadReplySeen.size?`已显示 ${threadReplySeen.size} 条直接回复${threadRepliesMore?'，可继续加载。':'。'}`:'暂时没有已归档的回复。';
  $('thread-replies-more').hidden=!threadRepliesMore;$('thread-replies-more').disabled=false;$('thread-replies-more').textContent='加载更多回复';
 }catch(err){
  if(isRequestAbort(err))return;
  if(activeThread(session,request,id)){$('thread-replies-status').textContent=err.message;$('thread-replies-more').hidden=false;$('thread-replies-more').disabled=false;$('thread-replies-more').textContent='重试加载回复';}
 }finally{if(session===postThreadSession)threadRepliesLoading=false;}
}
async function loadPostThread(id,request){
 if(!state.focusPost||!$('posts').querySelector(`article[data-post-id="${CSS.escape(id)}"]`))return;
 const session=++postThreadSession;threadContextNext=id;threadReplyCursor='';threadRepliesMore=true;
 threadContextSeen.clear();threadReplySeen.clear();
 $('posts').insertAdjacentHTML('beforeend','<div id="thread-replies-heading" class="thread-replies-heading"><h2>回复</h2><span>仅展示已归档内容</span></div><p id="thread-replies-status" class="thread-status" role="status">正在读取已归档回复…</p><button id="thread-replies-more" class="thread-load-more" type="button" hidden>加载更多回复</button>');
 await Promise.all([loadThreadContext(),loadThreadReplies()]);
 if(activeThread(session,request,id))await restorePostReturn();
}

$('posts').addEventListener('click',event=>{
 if(event.target.closest('#thread-context-more')){event.preventDefault();void loadThreadContext();}
 if(event.target.closest('#thread-replies-more')){event.preventDefault();void loadThreadReplies();}
});

const detailSurface=$('view-home'),detailFeed=$('home-feed');
detailSurface.addEventListener('pointerdown',event=>{
 if(!state.focusPost||!event.isPrimary||event.pointerType==='mouse'||event.button!==0)return;
 if(event.target.closest('a,button,input,select,video,audio,[role="slider"],[data-quote-post],.video-slot'))return;
 detailSwipe={id:event.pointerId,x:event.clientX,y:event.clientY,dx:0,axis:null,start:performance.now()};
 detailSurface.setPointerCapture(event.pointerId);
});
detailSurface.addEventListener('pointermove',event=>{
 const swipe=detailSwipe;if(!swipe||swipe.id!==event.pointerId)return;
 const dx=event.clientX-swipe.x,dy=event.clientY-swipe.y;
 if(!swipe.axis){if(Math.max(Math.abs(dx),Math.abs(dy))<10)return;swipe.axis=Math.abs(dx)>Math.abs(dy)*1.15?'horizontal':'vertical';}
 if(swipe.axis!=='horizontal'||dx<=0)return;
 event.preventDefault();swipe.dx=dx;detailFeed.classList.add('swiping');
 const offset=Math.min(innerWidth,dx);detailFeed.style.transform=`translate3d(${offset}px,0,0)`;detailFeed.style.opacity=String(1-Math.min(.35,offset/innerWidth*.35));
},{passive:false});
function finishDetailSwipe(event,cancel=false){
 const swipe=detailSwipe;if(!swipe||swipe.id!==event.pointerId)return;detailSwipe=null;
 if(detailSurface.hasPointerCapture(event.pointerId))detailSurface.releasePointerCapture(event.pointerId);
 const threshold=Math.min(180,Math.max(90,innerWidth*.22));
 const dismiss=!cancel&&swipe.axis==='horizontal'&&(swipe.dx>threshold||(swipe.dx>55&&performance.now()-swipe.start<240));
 detailFeed.classList.remove('swiping');detailFeed.classList.add('swipe-returning');detailSwipeSuppressUntil=performance.now()+350;
 if(dismiss){detailFeed.style.transform=`translate3d(${innerWidth}px,0,0)`;detailFeed.style.opacity='0';detailSwipeTimer=setTimeout(()=>leavePostDetail(),180);}
 else{detailFeed.style.removeProperty('transform');detailFeed.style.removeProperty('opacity');detailSwipeTimer=setTimeout(()=>detailFeed.classList.remove('swipe-returning'),180);}
}
detailSurface.addEventListener('pointerup',event=>finishDetailSwipe(event));
detailSurface.addEventListener('pointercancel',event=>finishDetailSwipe(event,true));
detailSurface.addEventListener('click',event=>{if(performance.now()<detailSwipeSuppressUntil){event.preventDefault();event.stopImmediatePropagation();}},true);
