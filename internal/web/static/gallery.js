'use strict';
let imageGallery=[],imageIndex=0,galleryDrag=null,galleryTimer=null,galleryBusy=false,galleryTarget=null,galleryClosing=false,galleryOverflow='';
let galleryTimeline=false,gallerySession=0,galleryLoading=false;
const galleryViewport=$('image-viewport'),galleryTrack=$('image-track');
function galleryPosition(offset=0,animate=false){
 galleryTrack.classList.toggle('animating',animate);
 galleryTrack.style.transform=`translate3d(calc(-100% + ${offset}px),0,0)`;
}
function galleryVerticalPosition(offset=0,animate=false){
 const progress=Math.min(1,Math.max(0,offset)/Math.max(1,galleryViewport.clientHeight));
 galleryViewport.classList.toggle('animating',animate);
 galleryViewport.style.transform=`translate3d(0,${offset}px,0) scale(${1-progress*.15})`;
 $('image-dialog').style.backgroundColor=`rgba(0,0,0,${1-progress*.8})`;
 $('image-count').style.opacity=String(1-progress);
}
function showGalleryImage(index){
 clearTimeout(galleryTimer);galleryTimer=null;galleryBusy=false;galleryTarget=null;galleryClosing=false;
 if(!imageGallery.length)return;
 imageIndex=Math.max(0,Math.min(index,imageGallery.length-1));
 for(const [id,i] of [['gallery-before',imageIndex-1],['large-image',imageIndex],['gallery-after',imageIndex+1]]){
  const img=$(id),src=imageGallery[i];
  if(src){if(img.getAttribute('src')!==src)img.src=src;}else img.removeAttribute('src');
 }
 $('large-image').alt=`已归档图片 ${imageIndex+1} / ${imageGallery.length}`;
 const multiple=imageGallery.length>1;
 $('image-count').hidden=!multiple;
 $('image-count').textContent=`${imageIndex+1} / ${imageGallery.length}`;
 galleryPosition();galleryVerticalPosition();
}
function openGallery(images,index=0,timeline=false){
 if(!images.length)return;
 gallerySession++;galleryTimeline=timeline;galleryLoading=false;
 imageGallery=images;galleryDrag=null;showGalleryImage(index);
 if(!$('image-dialog').open){galleryOverflow=document.body.style.overflow;document.body.style.overflow='hidden';$('image-dialog').showModal();$('image-dialog').focus({preventScroll:true});}
}
function settleGallery(delta=0){
 if(galleryBusy||!imageGallery.length)return;
 if(galleryLoading&&delta)return;
 if(galleryTimeline){
  imageGallery=[...$('posts').querySelectorAll('[data-image]')].map(item=>item.dataset.image);
  if(delta>0&&imageIndex===imageGallery.length-1&&state.hasMore){loadNextGalleryImage();return;}
 }
 const target=Math.max(0,Math.min(imageIndex+delta,imageGallery.length-1));
 delta=target-imageIndex;
 if(matchMedia('(prefers-reduced-motion: reduce)').matches){showGalleryImage(target);return;}
 galleryBusy=true;galleryTarget=target;
 // Commit the current drag position before animating to the adjacent slide.
 void galleryTrack.offsetWidth;
 galleryPosition(-delta*galleryViewport.clientWidth,true);
 galleryVerticalPosition(0,true);
 galleryTimer=setTimeout(()=>showGalleryImage(target),240);
}
function closeTimelineGallery(){if($('image-dialog').open)$('image-dialog').close();}
async function loadNextGalleryImage(){
 if(galleryLoading)return;
 const session=gallerySession,current=$('posts').querySelectorAll('[data-image]')[imageIndex];
 if(!current)return;
 galleryLoading=true;galleryPosition(0,true);
 $('image-count').hidden=false;$('image-count').textContent='正在查找下一张图片…';
 const next=await nextTimelineMedia(current,'[data-image]',()=>session===gallerySession&&$('image-dialog').open);
 if(session!==gallerySession||!$('image-dialog').open)return;
 galleryLoading=false;
 showGalleryImage(imageIndex);
 if(next)settleGallery(1);
}
function dismissGallery(){
 gallerySession++;galleryLoading=false;
 if(matchMedia('(prefers-reduced-motion: reduce)').matches){$('image-dialog').close();return;}
 galleryBusy=true;galleryClosing=true;
 galleryVerticalPosition(galleryViewport.clientHeight,true);
 clearTimeout(galleryTimer);galleryTimer=setTimeout(()=>$('image-dialog').close(),240);
}
function stepGallery(delta){if(!galleryDrag)settleGallery(delta);}
function cancelGalleryDrag(){galleryDrag=null;if(!galleryBusy&&imageGallery.length)settleGallery();}
galleryViewport.addEventListener('pointerdown',event=>{
 if(!event.isPrimary){cancelGalleryDrag();return;}
 if(galleryBusy||!imageGallery.length||event.button!==0||(window.visualViewport?.scale||1)>1.01)return;
 galleryDrag={id:event.pointerId,x:event.clientX,y:event.clientY,dx:0,dy:0,axis:null,start:performance.now()};
 galleryViewport.setPointerCapture(event.pointerId);
});
galleryViewport.addEventListener('pointermove',event=>{
 const drag=galleryDrag;if(!drag||drag.id!==event.pointerId)return;
 const dx=event.clientX-drag.x,dy=event.clientY-drag.y;
 if(!drag.axis){
  if(Math.max(Math.abs(dx),Math.abs(dy))<8)return;
  drag.axis=Math.abs(dy)>Math.abs(dx)?'vertical':'horizontal';
 }
 drag.dx=dx;drag.dy=dy;
 if(drag.axis==='vertical'){galleryVerticalPosition(dy>0?dy:dy*.2);return;}
 const edge=(imageIndex===0&&dx>0)||(imageIndex===imageGallery.length-1&&dx<0);
 galleryPosition(Math.max(-galleryViewport.clientWidth,Math.min(galleryViewport.clientWidth,dx))*(edge ? .25 : 1));
});
galleryViewport.addEventListener('pointerup',event=>{
 const drag=galleryDrag;if(!drag||drag.id!==event.pointerId)return;
 galleryDrag=null;
 if(galleryViewport.hasPointerCapture(event.pointerId))galleryViewport.releasePointerCapture(event.pointerId);
 if(drag.axis==='vertical'){
  const threshold=Math.min(160,Math.max(80,galleryViewport.clientHeight*.18));
  if(drag.dy>threshold||(drag.dy>48&&performance.now()-drag.start<220))dismissGallery();else settleGallery();
  return;
 }
 const threshold=Math.min(80,Math.max(40,galleryViewport.clientWidth*.18));
 const swipe=drag.axis==='horizontal'&&(Math.abs(drag.dx)>threshold||(Math.abs(drag.dx)>24&&performance.now()-drag.start<220));
 settleGallery(swipe?(drag.dx<0?1:-1):0);
});
galleryViewport.addEventListener('pointercancel',cancelGalleryDrag);
galleryViewport.addEventListener('lostpointercapture',()=>{if(galleryDrag)cancelGalleryDrag();});
galleryViewport.addEventListener('dragstart',event=>event.preventDefault());
$('image-dialog').addEventListener('keydown',event=>{
 if(event.key==='ArrowLeft'){event.preventDefault();stepGallery(-1);}
 if(event.key==='ArrowRight'){event.preventDefault();stepGallery(1);}
});
window.addEventListener('resize',()=>{galleryDrag=null;if($('image-dialog').open){if(galleryClosing)$('image-dialog').close();else showGalleryImage(galleryTarget??imageIndex);}});
window.visualViewport?.addEventListener('resize',()=>{galleryViewport.style.touchAction=window.visualViewport.scale>1.01?'auto':'pinch-zoom';});
$('image-dialog').addEventListener('close',()=>{
 gallerySession++;galleryTimeline=false;galleryLoading=false;
 clearTimeout(galleryTimer);galleryTimer=null;galleryBusy=false;galleryTarget=null;galleryDrag=null;galleryClosing=false;
 imageGallery=[];imageIndex=0;galleryPosition();galleryVerticalPosition();
 for(const id of ['large-image','gallery-before','gallery-after'])$(id).removeAttribute('src');
 $('large-image').alt='已归档图片';document.body.style.overflow=galleryOverflow;
});
