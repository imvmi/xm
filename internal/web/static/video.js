'use strict';
let videoPlayRequest=0,expandedVideoShell=null,videoFullscreenRequest=0,videoPageOverflow='';
let videoDrag=null,videoTap=null,videoSwipePending=false,videoSuppressClick=false;
function videoIcon(name){
 const paths={play:'<path d="m8 5 11 7-11 7Z" fill="currentColor" stroke="none"/>',pause:'<path d="M7 5h4v14H7zm6 0h4v14h-4z" fill="currentColor" stroke="none"/>',muted:'<path d="M11 5 6 9H3v6h3l5 4zM16 9l6 6m0-6-6 6"/>',sound:'<path d="M11 5 6 9H3v6h3l5 4zM15 8a6 6 0 0 1 0 8m3-11a10 10 0 0 1 0 14"/>',expand:'<path d="M9 3H3v6m12-6h6v6M3 15v6h6m12-6v6h-6"/>',collapse:'<path d="M3 9h6V3m12 6h-6V3M9 21v-6H3m12 6v-6h6"/>'};
 return `<svg viewBox="0 0 24 24" aria-hidden="true">${paths[name]||paths.play}</svg>`;
}
function videoHTML(post,src){
 return `<div class="video-slot"><div class="video-shell"><button type="button" class="video-load" data-video-src="${escape(src)}" aria-label="全屏播放 @${escape(post.username)} 的归档原视频"><span aria-hidden="true">▶</span><strong>播放原视频</strong><small>点击全屏播放 · 原始画质</small></button><video playsinline preload="none" tabindex="0" role="button" hidden aria-label="点击全屏显示视频" aria-expanded="false"></video><span class="video-loading" role="status" hidden>正在加载…</span><div class="video-controls" hidden><button type="button" data-video-action="play" aria-label="播放">${videoIcon('play')}</button><input class="video-seek" type="range" min="0" max="1000" step="1" value="0" aria-label="视频播放进度" disabled><span class="video-time">0:00 / 0:00</span><button type="button" data-video-action="mute" aria-label="开启声音" aria-pressed="true">${videoIcon('muted')}</button><button type="button" data-video-action="fullscreen" aria-label="全屏显示">${videoIcon('expand')}</button></div></div></div>`;
}
function videoTime(seconds){
 const value=Number.isFinite(seconds)?Math.max(0,Math.floor(seconds)):0;
 const hours=Math.floor(value/3600),minutes=Math.floor(value/60)%60;
 return (hours?hours+':'+String(minutes).padStart(2,'0'):Math.floor(value/60))+':'+String(value%60).padStart(2,'0');
}
function syncVideoControls(video){
 const shell=video.closest('.video-shell');if(!shell)return;
 const loaded=!!video.getAttribute('src'),paused=video.paused||video.ended;
 shell.querySelector('.video-controls').hidden=!loaded;
 const play=shell.querySelector('[data-video-action="play"]');
 play.innerHTML=videoIcon(paused?'play':'pause');play.setAttribute('aria-label',paused?'播放':'暂停');
 const muted=video.muted||video.volume===0,mute=shell.querySelector('[data-video-action="mute"]');
 mute.innerHTML=videoIcon(muted?'muted':'sound');mute.setAttribute('aria-label',muted?'开启声音':'静音');mute.setAttribute('aria-pressed',String(muted));
 const seek=shell.querySelector('.video-seek'),duration=video.duration;
 seek.disabled=!Number.isFinite(duration)||duration<=0;
 seek.value=seek.disabled?0:Math.round(video.currentTime/duration*1000);
 shell.querySelector('.video-time').textContent=videoTime(video.currentTime)+' / '+videoTime(duration);
 const loading=shell.querySelector('.video-loading'),navigating=videoSwipePending&&shell===expandedVideoShell;
 loading.textContent=navigating?'正在查找下一条视频…':'正在加载…';
 loading.hidden=!navigating&&(!loaded||paused||video.readyState>=3);
 const expanded=shell===expandedVideoShell,full=shell.querySelector('[data-video-action="fullscreen"]');
 full.innerHTML=videoIcon(expanded?'collapse':'expand');full.setAttribute('aria-label',expanded?'返回时间线内嵌视频':'全屏显示');
 video.setAttribute('aria-label',expanded?'上滑观看下一条视频，下滑返回时间线':'点击全屏显示视频');video.setAttribute('aria-expanded',String(expanded));
}
function collapseExpandedVideo(restoreFocus=true){
 const shell=expandedVideoShell;if(!shell)return;
 resetVideoDrag();videoTap=null;videoSwipePending=false;
 expandedVideoShell=null;videoFullscreenRequest++;
 shell.classList.remove('video-expanded');shell.removeAttribute('role');shell.removeAttribute('aria-modal');shell.removeAttribute('aria-label');delete shell.dataset.nativeFullscreen;
 document.body.style.overflow=videoPageOverflow;
 if(document.fullscreenElement===shell)document.exitFullscreen().catch(()=>{});
 const video=shell.querySelector('video');syncVideoControls(video);
 if(restoreFocus&&video.isConnected)video.focus({preventScroll:true});
}
function expandTimelineVideo(video){
 const shell=video.closest('.video-shell');if(!shell||expandedVideoShell===shell)return;
 if(expandedVideoShell)collapseExpandedVideo(false);
 const request=++videoFullscreenRequest;
 expandedVideoShell=shell;videoPageOverflow=document.body.style.overflow;document.body.style.overflow='hidden';
 shell.classList.add('video-expanded');shell.setAttribute('role','dialog');shell.setAttribute('aria-modal','true');shell.setAttribute('aria-label','视频播放器');
 syncVideoControls(video);video.focus({preventScroll:true});
 // Keep the video in place (and keep its currentTime). iPhone/denied fullscreen
 // uses the fixed viewport layout instead of the OS player that intercepts taps.
 if(shell.requestFullscreen&&document.fullscreenEnabled){
  try{Promise.resolve(shell.requestFullscreen({navigationUI:'hide'})).then(()=>{
   if(request!==videoFullscreenRequest||expandedVideoShell!==shell){if(document.fullscreenElement===shell)document.exitFullscreen().catch(()=>{});}
  }).catch(()=>{});}catch{}
 }
}
function toggleVideoFullscreen(video){if(expandedVideoShell===video.closest('.video-shell'))collapseExpandedVideo();else expandTimelineVideo(video);}
function resetTimelineVideo(video,failed=false){
 const shell=video.closest('.video-shell');
 if(shell===expandedVideoShell)collapseExpandedVideo(false);
 delete video.dataset.playRequest;
 video.pause();video.removeAttribute('src');video.load();video.hidden=true;
 const button=shell?.querySelector('[data-video-src]');
 if(button){button.hidden=false;button.querySelector('strong').textContent=failed?'重试播放':'播放原视频';button.querySelector('small').textContent=failed?'视频加载失败，点击重试':'点击全屏播放 · 原始画质';}
 syncVideoControls(video);
}
function releaseTimelineMedia(){
 videoTap=null;
 if(typeof closeTimelineGallery==='function')closeTimelineGallery();
 if(expandedVideoShell)collapseExpandedVideo(false);
 for(const video of document.querySelectorAll('#posts video[src]'))resetTimelineVideo(video);
}
function playTimelineVideo(video){
 const request=video.dataset.playRequest;
 video.play().catch(err=>{
  if(!video.isConnected||video.dataset.playRequest!==request||!video.getAttribute('src')||err.name==='AbortError')return;
  if(err.name==='NotAllowedError'){syncVideoControls(video);toast('请点击下方播放按钮继续播放');return;}
  resetTimelineVideo(video,true);
 });
 syncVideoControls(video);
}
function startTimelineVideo(button){
 const video=button.closest('.video-shell')?.querySelector('video');if(!video||video.getAttribute('src'))return;
 video.dataset.playRequest=String(++videoPlayRequest);
 video.muted=defaultVideoMuted();video.loop=state.settings?.video_loop===true;
 button.hidden=true;video.hidden=false;video.src=button.dataset.videoSrc;
 playTimelineVideo(video);expandTimelineVideo(video);
}
$('posts').addEventListener('click',event=>{
 const load=event.target.closest('[data-video-src]');if(load){event.preventDefault();startTimelineVideo(load);return;}
 const shell=event.target.closest('.video-shell');if(!shell)return;
 const video=shell.querySelector('video'),button=event.target.closest('[data-video-action]');
 if(button){
  event.preventDefault();
  switch(button.dataset.videoAction){
   case 'play':if(video.paused||video.ended){if(video.ended)video.currentTime=0;playTimelineVideo(video);}else video.pause();break;
   case 'mute':video.muted=!(video.muted||video.volume===0);if(!video.muted&&video.volume===0)video.volume=1;break;
   case 'fullscreen':toggleVideoFullscreen(video);break;
  }
  syncVideoControls(video);return;
 }
 if(event.target===video&&!videoSuppressClick)expandTimelineVideo(video);
});
function resetVideoDrag(){
 const drag=videoDrag;videoDrag=null;
 if(drag&&drag.shell.hasPointerCapture(drag.id))drag.shell.releasePointerCapture(drag.id);
 const video=expandedVideoShell?.querySelector('video');
 if(video)video.style.transform='';
}
async function nextExpandedVideo(){
 if(videoSwipePending||!expandedVideoShell)return;
 const shell=expandedVideoShell,request=videoFullscreenRequest;
 const active=()=>expandedVideoShell===shell&&request===videoFullscreenRequest;
 videoSwipePending=true;
 syncVideoControls(shell.querySelector('video'));
 const next=await nextTimelineMedia(shell,'.video-shell',active);
 if(!active())return;
 if(!next){videoSwipePending=false;syncVideoControls(shell.querySelector('video'));if(!state.postsError)toast('已到当前时间线的最后一条视频');return;}
 // Finish the old native fullscreen exit before opening the next player, so
 // its fullscreenchange event cannot collapse the new player.
 if(document.fullscreenElement===shell){
  delete shell.dataset.nativeFullscreen;
  try{await document.exitFullscreen();}catch{}
  if(!active())return;
 }
 shell.querySelector('video').pause();collapseExpandedVideo(false);
 const video=next.querySelector('video');
 if(video.getAttribute('src')){playTimelineVideo(video);expandTimelineVideo(video);}
 else startTimelineVideo(next.querySelector('[data-video-src]'));
}
$('posts').addEventListener('pointerdown',event=>{
 videoTap=null;
 if(!event.isPrimary){resetVideoDrag();return;}
 videoSuppressClick=false;
 const shell=event.target.closest('.video-shell');
 const tapTarget=event.target.closest('video,[data-video-action],[data-video-src]');
 if(shell&&tapTarget&&event.pointerType==='touch')videoTap={target:tapTarget,x:event.clientX,y:event.clientY};
 if(!shell||shell!==expandedVideoShell||event.button!==0||event.target.closest('.video-controls')||(window.visualViewport?.scale||1)>1.01)return;
 videoDrag={shell,id:event.pointerId,x:event.clientX,y:event.clientY,dy:0,axis:null,start:performance.now()};
 shell.setPointerCapture(event.pointerId);
});
$('posts').addEventListener('pointermove',event=>{
 if(videoTap&&Math.max(Math.abs(event.clientX-videoTap.x),Math.abs(event.clientY-videoTap.y))>8)videoTap=null;
 const drag=videoDrag;if(!drag||drag.id!==event.pointerId)return;
 const dx=event.clientX-drag.x,dy=event.clientY-drag.y;
 if(!drag.axis){if(Math.max(Math.abs(dx),Math.abs(dy))<8)return;drag.axis=Math.abs(dy)>Math.abs(dx)?'vertical':'horizontal';}
 drag.dy=dy;videoSuppressClick=true;
 if(drag.axis==='vertical')drag.shell.querySelector('video').style.transform=`translate3d(0,${dy}px,0)`;
});
$('posts').addEventListener('pointerup',event=>{
 const drag=videoDrag;if(!drag||drag.id!==event.pointerId)return;
 resetVideoDrag();
 if(drag.axis!=='vertical')return;
 const distance=Math.abs(drag.dy),threshold=Math.min(160,Math.max(80,drag.shell.clientHeight*.18));
 if(distance>threshold||(distance>48&&performance.now()-drag.start<220)){
  // Complete touchend before changing the touched element's fullscreen layout.
  const request=videoFullscreenRequest;
  setTimeout(()=>{if(expandedVideoShell===drag.shell&&videoFullscreenRequest===request){if(drag.dy>0)collapseExpandedVideo();else void nextExpandedVideo();}},0);
 }
});
$('posts').addEventListener('pointercancel',()=>{videoTap=null;resetVideoDrag();});
$('posts').addEventListener('lostpointercapture',()=>{if(videoDrag)resetVideoDrag();});
// Handle stationary touches explicitly: browsers can suppress their synthetic
// click after a swipe. Prevent that click so play/mute buttons only fire once.
$('posts').addEventListener('touchend',event=>{
 const tap=videoTap;videoTap=null;
 if(!tap||!tap.target.isConnected||event.touches.length)return;
 event.preventDefault();tap.target.click();
},{passive:false});
$('posts').addEventListener('input',event=>{
 if(!event.target.matches('.video-seek'))return;
 const video=event.target.closest('.video-shell').querySelector('video');
 if(Number.isFinite(video.duration)&&video.duration>0){video.currentTime=Number(event.target.value)/1000*video.duration;syncVideoControls(video);}
});
for(const name of ['play','playing','pause','ended','timeupdate','loadedmetadata','durationchange','volumechange','waiting','canplay']){
 $('posts').addEventListener(name,event=>{
  if(!event.target.matches('video'))return;
  if(name==='play')for(const other of document.querySelectorAll('#posts video[src]'))if(other!==event.target)other.pause();
  syncVideoControls(event.target);
 },true);
}
$('posts').addEventListener('error',event=>{if(event.target.matches('video[src]'))resetTimelineVideo(event.target,true);},true);
$('posts').addEventListener('keydown',event=>{
 if(event.target.matches('video')&&(event.key==='Enter'||event.key===' ')){event.preventDefault();expandTimelineVideo(event.target);}
});
document.addEventListener('fullscreenchange',()=>{
 if(!expandedVideoShell)return;
 if(document.fullscreenElement===expandedVideoShell)expandedVideoShell.dataset.nativeFullscreen='true';
 else if(expandedVideoShell.dataset.nativeFullscreen)collapseExpandedVideo();
});
document.addEventListener('keydown',event=>{
 if(!expandedVideoShell)return;
 if(event.key==='Escape'){event.preventDefault();collapseExpandedVideo();return;}
 if(event.target.matches('video')&&event.key==='ArrowUp'){event.preventDefault();void nextExpandedVideo();return;}
 if(event.target.matches('video')&&event.key==='ArrowDown'){event.preventDefault();collapseExpandedVideo();return;}
 if(event.key==='Tab'){
  const items=[...expandedVideoShell.querySelectorAll('video,button:not([hidden]),input:not(:disabled)')].filter(el=>el.getClientRects().length);
  const first=items[0],last=items.at(-1);
  if(event.shiftKey&&(document.activeElement===first||!expandedVideoShell.contains(document.activeElement))){event.preventDefault();last?.focus();}
  else if(!event.shiftKey&&(document.activeElement===last||!expandedVideoShell.contains(document.activeElement))){event.preventDefault();first?.focus();}
 }
});
window.addEventListener('pagehide',releaseTimelineMedia);
