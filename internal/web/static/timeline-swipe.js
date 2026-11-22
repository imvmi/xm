'use strict';
(() => {
  const panel=$('timeline-panel');
  const reducedMotion=matchMedia('(prefers-reduced-motion: reduce)');
  const controls='a,button,input,select,textarea,video,audio,[role="slider"],[contenteditable="true"],.video-slot';
  let gesture=null,animation=null,feed=null,sequence=0,suppressClickUntil=0;
  const tabs=()=>panel.parentElement.querySelector('.timeline-tabs');
  const names=()=>state.view==='home'?homeTimelineTabs:timelineTabs;
  const enabled=()=>state.authenticated&&!state.focusPost&&['home','user'].includes(state.view)&&!document.querySelector('dialog[open],.video-expanded')&&!document.fullscreenElement;
  const transform=x=>`translate3d(${x}px,0,0)`;

  function releaseGesture(){
    const previous=gesture;gesture=null;
    if(previous&&panel.hasPointerCapture(previous.id))panel.releasePointerCapture(previous.id);
  }
  function reset(){
    sequence++;
    animation?.cancel();animation=null;
    releaseGesture();
    panel.style.removeProperty('transform');
    panel.style.removeProperty('min-height');
    panel.inert=false;
    feed?.classList.remove('timeline-sliding','timeline-dragging');feed=null;
    if(['home','user'].includes(state.view))tabs()?.style.setProperty('--tab-index',names().indexOf(state.timelineTab));
  }
  window.cancelTimelineSwipe=reset;

  async function slide(from,to,duration){
    if(reducedMotion.matches)return;
    const current=panel.animate([{transform:transform(from)},{transform:transform(to)}],{
      duration,easing:'cubic-bezier(.2,.7,.2,1)',fill:'forwards'
    });
    animation?.cancel();animation=current;
    try{await current.finished;}catch(_){/* Navigation or another tab superseded this slide. */}
  }
  async function settle(from){
    reset();
    if(!from||reducedMotion.matches)return;
    const token=sequence;
    feed=panel.parentElement;feed.classList.add('timeline-sliding');
    await slide(from,0,180);
    if(token===sequence)reset();
  }
  window.animateTimelineTab=async (name,home,apply)=>{
    const from=gesture?.dx||0;
    const previous=state.timelineTab;
    reset();
    if(name===previous){await settle(from);return;}
    if(!enabled())return;
    const token=sequence,route=state.navRequest;
    const order=home?homeTimelineTabs:timelineTabs;
    const direction=Math.sign(order.indexOf(name)-order.indexOf(previous));
    const width=panel.getBoundingClientRect().width;
    feed=panel.parentElement;feed.classList.add('timeline-sliding');
    panel.inert=true;
    // Keep the visible viewport stable while the next list is loading. Avoid
    // retaining the height of a long, infinitely loaded timeline.
    const top=panel.getBoundingClientRect().top;
    panel.style.minHeight=Math.max(0,innerHeight-Math.max(0,top))+'px';
    tabs().style.setProperty('--tab-index',order.indexOf(name));
    await slide(from,-direction*width,110);
    if(token!==sequence)return;
    if(route!==state.navRequest||!enabled()){reset();return;}
    // If switching far down a list, return to its start without hiding the tabs.
    const header=feed.querySelector('.profile-top');
    const tabTop=tabs().getBoundingClientRect().top;
    const headerBottom=header.getBoundingClientRect().bottom;
    if(tabTop<headerBottom)window.scrollBy({top:tabTop-headerBottom,behavior:'instant'});
    apply();
    await slide(direction*width,0,230);
    if(token===sequence)reset();
  };

  panel.addEventListener('pointerdown',event=>{
    // A second finger cancels the drag so native pinch zoom remains available.
    if(!event.isPrimary){if(gesture)reset();return;}
    if(!enabled()||animation||event.pointerType==='mouse'||event.button!==0||event.target.closest(controls)||!getSelection().isCollapsed)return;
    gesture={id:event.pointerId,x:event.clientX,y:event.clientY,dx:0,axis:'',start:performance.now(),width:panel.getBoundingClientRect().width};
  });
  panel.addEventListener('pointermove',event=>{
    if(!gesture||event.pointerId!==gesture.id)return;
    const dx=event.clientX-gesture.x,dy=event.clientY-gesture.y;
    if(!gesture.axis){
      if(Math.max(Math.abs(dx),Math.abs(dy))<10)return;
      if(Math.abs(dx)<=Math.abs(dy)*1.2){releaseGesture();return;}
      gesture.axis='horizontal';
      panel.setPointerCapture(event.pointerId);
      feed=panel.parentElement;feed.classList.add('timeline-sliding','timeline-dragging');
    }
    if(event.cancelable)event.preventDefault();
    suppressClickUntil=performance.now()+400;
    const index=names().indexOf(state.timelineTab);
    const edge=(dx>0&&index===0)||(dx<0&&index===names().length-1);
    gesture.dx=Math.max(-gesture.width,Math.min(gesture.width,dx))*(edge ? .18 : 1);
    if(!reducedMotion.matches){
      panel.style.transform=transform(gesture.dx);
      tabs().style.setProperty('--tab-index',Math.max(0,Math.min(names().length-1,index-gesture.dx/gesture.width)));
    }
  },{passive:false});
  function end(event,cancelled=false){
    if(!gesture||event.pointerId!==gesture.id)return;
    const {axis,dx,width,start}=gesture;
    if(axis!=='horizontal'){releaseGesture();return;}
    suppressClickUntil=performance.now()+400;
    const index=names().indexOf(state.timelineTab),next=index+(dx<0?1:-1);
    const far=Math.abs(dx)>=Math.min(120,width*.23);
    const flick=Math.abs(dx)>45&&performance.now()-start<250;
    if(!cancelled&&(far||flick)&&next>=0&&next<names().length)selectTimelineTab(names()[next]);
    else settle(dx);
  }
  panel.addEventListener('pointerup',event=>end(event));
  panel.addEventListener('pointercancel',event=>end(event,true));
  // Moving implicit touch capture from the original text node to the panel
  // also bubbles a lost-capture event; that is not a cancelled gesture.
  panel.addEventListener('lostpointercapture',event=>{if(event.target===panel)end(event,true);});
  panel.addEventListener('click',event=>{
    if(performance.now()<suppressClickUntil){event.preventDefault();event.stopImmediatePropagation();}
  },true);
  // Route and auth changes reset synchronously in app.js. Resize/rotation and
  // backgrounding also must not leave a translated or inert timeline behind.
  window.addEventListener('resize',reset);
  document.addEventListener('visibilitychange',()=>{if(document.hidden)reset();});
  reducedMotion.addEventListener('change',reset);
})();
