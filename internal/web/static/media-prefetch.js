'use strict';
let mediaPrefetchJob=null;
function mediaPreloadMode(){return state.settings?.media_preload_mode||'standard';}
function mediaViewerOpen(){return $('image-dialog').open||!!expandedVideoShell;}
function canPrefetchMedia(){return mediaPreloadMode()!=='off'&&!document.hidden;}
// A navigation action starts at most two sequential page requests. No recursive
// refill on append: media-sparse timelines must not drain the entire archive.
function prefetchMediaPages(current,selector,threshold,isCurrent){
 if(!canPrefetchMedia()||!isCurrent())return Promise.resolve();
 if(mediaPrefetchJob?.active())return mediaPrefetchJob.promise;
 const request=state.postRequest,active=()=>request===state.postRequest&&canPrefetchMedia()&&isCurrent();
 const job={active,promise:null};
 job.promise=(async()=>{
  for(let pages=0;pages<2&&active()&&state.hasMore&&!state.postsError;pages++){
   const node=current(),items=[...$('posts').querySelectorAll(selector)],index=items.indexOf(node);
   if(index<0||!node.isConnected||items.length-index-1>threshold)return;
   const offset=state.offset;
   await loadMorePosts({background:true,isCurrent:active});
   if(state.offset===offset)return;
  }
 })().finally(()=>{if(mediaPrefetchJob===job)mediaPrefetchJob=null;});
 mediaPrefetchJob=job;return job.promise;
}

let galleryPrefetchImages=new Map();
function clearGalleryPrefetch(){
 for(const img of galleryPrefetchImages.values())img.removeAttribute('src');
 galleryPrefetchImages.clear();
}
function warmGalleryImages(){
 const main=$('large-image');
 if(!canPrefetchMedia()||!$('image-dialog').open||!main.complete||!main.naturalWidth){clearGalleryPrefetch();return;}
 const wanted=new Set(imageGallery.slice(imageIndex+1,imageIndex+3));
 for(const [src,img] of galleryPrefetchImages)if(!wanted.has(src)){img.removeAttribute('src');galleryPrefetchImages.delete(src);}
 for(const src of wanted)if(!galleryPrefetchImages.has(src)){
  const img=new Image();img.fetchPriority='low';img.decoding='async';img.src=src;
  galleryPrefetchImages.set(src,img);
  if(img.decode)img.decode().catch(()=>{});
 }
}
function requestGalleryPrefetch(){
 warmGalleryImages();
 if(!galleryTimeline||!$('image-dialog').open)return;
 const session=gallerySession,request=state.postRequest;
 const active=()=>request===state.postRequest&&session===gallerySession&&galleryTimeline&&$('image-dialog').open;
 prefetchMediaPages(()=>$('posts').querySelectorAll('[data-image]')[imageIndex],'[data-image]',3,active).then(()=>{
  if(!active()||!canPrefetchMedia())return;
  imageGallery=[...$('posts').querySelectorAll('[data-image]')].map(item=>item.dataset.image);
  refreshGalleryNeighbors();warmGalleryImages();
 });
}

let prefetchedVideo=null,videoPrefetchOwner=null,videoPrefetchAttempt=null;
function clearVideoPrefetch(keep=null,failed=false){
 const video=prefetchedVideo;
 if(!video||video===keep)return;
 prefetchedVideo=null;
 if(!failed&&videoPrefetchAttempt===video)videoPrefetchAttempt=null;
 if(video.dataset.prefetch){delete video.dataset.prefetch;video.pause();video.removeAttribute('src');video.preload='none';video.load();}
}
function claimVideoPrefetch(video){
 if(prefetchedVideo===video)prefetchedVideo=null;
 delete video.dataset.prefetch;
}
function resetVideoPrefetch(){clearVideoPrefetch();videoPrefetchOwner=null;videoPrefetchAttempt=null;}
function warmNextVideo(shell,active){
 if(!active()||!canPrefetchMedia()){clearVideoPrefetch();return;}
 const shells=[...$('posts').querySelectorAll('.video-shell')],index=shells.indexOf(shell),next=shells[index+1];
 const video=next?.querySelector('video');
 if(index<0||!video){clearVideoPrefetch();return;}
 if(prefetchedVideo===video)return;
 clearVideoPrefetch();
 if(video.getAttribute('src')||videoPrefetchAttempt===video)return;
 videoPrefetchAttempt=video;prefetchedVideo=video;
 video.dataset.prefetch='true';video.preload=mediaPreloadMode()==='enhanced'?'auto':'metadata';
 video.src=next.querySelector('[data-video-src]').dataset.videoSrc;video.load();
}
function requestVideoPrefetch(){
 const shell=expandedVideoShell,video=shell?.querySelector('video');
 if(!video)return;
 const request=videoFullscreenRequest,timeline=state.postRequest;
 const active=()=>timeline===state.postRequest&&expandedVideoShell===shell&&videoFullscreenRequest===request&&!video.paused&&!video.ended&&video.readyState>=3;
 if(!active()||!canPrefetchMedia()){clearVideoPrefetch();return;}
 if(videoPrefetchOwner===video){warmNextVideo(shell,active);return;}
 videoPrefetchOwner=video;videoPrefetchAttempt=null;
 warmNextVideo(shell,active);
 prefetchMediaPages(()=>shell,'.video-shell',2,active).then(()=>{
  if(expandedVideoShell===shell&&videoFullscreenRequest===request)warmNextVideo(shell,active);
 });
}
$('large-image').addEventListener('load',warmGalleryImages);
document.addEventListener('visibilitychange',()=>{
 if(document.hidden){clearGalleryPrefetch();clearVideoPrefetch();if($('image-dialog').open)refreshGalleryNeighbors();}
 else {if($('image-dialog').open){refreshGalleryNeighbors();warmGalleryImages();}requestVideoPrefetch();}
});
