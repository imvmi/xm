const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const state={settings:{}},plays=[],notices=[],events={},postEvents={},documentEvents={};
 const element=()=>({dataset:{},attributes:{},style:{},hidden:false,innerHTML:'',textContent:'',
  classList:{values:new Set(),add(v){this.values.add(v)},remove(v){this.values.delete(v)},contains(v){return this.values.has(v)}},
  setAttribute(k,v){this.attributes[k]=v},removeAttribute(k){delete this.attributes[k]},focus(){}});
 const strong=element(),small=element(),controls=element(),play=element(),mute=element(),full=element(),seek=element(),time=element(),loading=element();
 const video=Object.assign(element(),{hidden:true,isConnected:true,muted:false,volume:1,paused:true,ended:false,duration:100,currentTime:0,readyState:4,src:'',pauses:0,loads:0,getAttribute(k){return k==='src'?this.src:this.attributes[k]},removeAttribute(k){if(k==='src')this.src='';else delete this.attributes[k]},pause(){this.pauses++;this.paused=true},load(){this.loads++},play(){this.paused=false;return new Promise((resolve,reject)=>plays.push({resolve,reject,muted:this.muted}))},closest(q){return q==='.video-shell'?shell:null}});
 const button=Object.assign(element(),{dataset:{videoSrc:'/media/100/asset?v=hash'},closest(){return shell},querySelector(q){return q==='strong'?strong:small}});
 const shell=Object.assign(element(),{querySelector(q){return {'video':video,'[data-video-src]':button,'.video-controls':controls,'[data-video-action="play"]':play,'[data-video-action="mute"]':mute,'[data-video-action="fullscreen"]':full,'.video-seek':seek,'.video-time':time,'.video-loading':loading}[q]}});
 const document={body:{style:{overflow:'auto'}},querySelectorAll:()=>video.src?[video]:[],addEventListener:(k,fn)=>documentEvents[k]=fn,fullscreenEnabled:false,fullscreenElement:null,exitFullscreen(){this.fullscreenElement=null;return Promise.resolve()}};
 const ctx=vm.createContext({state,document,$:()=>({addEventListener:(k,fn)=>postEvents[k]=fn}),window:{addEventListener:(k,fn)=>events[k]=fn},escape:v=>String(v).replaceAll('&','&amp;').replaceAll('"','&quot;'),icon:()=>'',toast:m=>notices.push(m)});
 vm.runInContext(source.slice(source.indexOf('function defaultVideoMuted()'),source.indexOf('function displayPostText(')),ctx);
 vm.runInContext(fs.readFileSync(path.join(__dirname,'../static/video.js'),'utf8'),ctx);
 return {ctx,state,video,button,shell,controls,play,mute,full,seek,document,plays,strong,small,events,postEvents,documentEvents,notices};
}
const tick=()=>new Promise(resolve=>setImmediate(resolve));
test('media renders original versioned images and videos without src or posters',()=>{
 const f=fixture(),html=f.ctx.mediaHTML({id:'100',username:'alice',media:[{id:'p',kind:'photo',status:'done',version:'abcd'},{id:'v',kind:'video',status:'done',version:'efgh'}]});
 assert.match(html,/<img src="\/media\/100\/p\?v=abcd" loading="lazy"/);
 assert.match(html,/data-image="\/media\/100\/p\?v=abcd"/);
 assert.match(html,/data-video-src="\/media\/100\/v\?v=efgh"/);
 assert.match(html,/<video[^>]*preload="none"[^>]*hidden/);
 assert.doesNotMatch(html,/<video[^>]*\ssrc=|poster=|thumbnail/);
});
test('play attaches the source once and defaults to muted; manual unmute persists',async()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);f.ctx.startTimelineVideo(f.button);
 assert.equal(f.plays.length,1);assert.equal(f.video.src,f.button.dataset.videoSrc);assert.equal(f.video.hidden,false);assert.equal(f.plays[0].muted,true);
 f.video.muted=false;f.plays[0].resolve();await tick();assert.equal(f.video.muted,false);
 f.ctx.releaseTimelineMedia();assert.equal(f.video.src,'');assert.equal(f.button.hidden,false);assert.ok(f.video.pauses>0);assert.ok(f.video.loads>0);
 f.ctx.startTimelineVideo(f.button);assert.equal(f.plays[1].muted,true);f.plays[1].resolve();await tick();
});
test('saved sound preference applies to new playback without overriding current playback',async()=>{
 const f=fixture();f.state.settings.video_default_muted=false;f.ctx.startTimelineVideo(f.button);
 assert.equal(f.plays[0].muted,false);f.state.settings.video_default_muted=true;f.plays[0].resolve();await tick();assert.equal(f.video.muted,false);
 f.events.pagehide();assert.equal(f.video.src,'');f.ctx.startTimelineVideo(f.button);assert.equal(f.plays[1].muted,true);f.plays[1].resolve();await tick();
});
test('loop preference defaults off and is applied on each new playback',()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);assert.equal(f.video.loop,false);
 f.ctx.releaseTimelineMedia();f.state.settings.video_loop=true;f.ctx.startTimelineVideo(f.button);assert.equal(f.video.loop,true);
 f.ctx.releaseTimelineMedia();f.state.settings.video_loop=false;f.ctx.startTimelineVideo(f.button);assert.equal(f.video.loop,false);
});
test('failed playback offers retry; stale rejected playback cannot reset the new attempt',async()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);f.plays[0].reject(new Error('network'));await tick();
 assert.equal(f.video.src,'');assert.equal(f.button.hidden,false);assert.equal(f.strong.textContent,'重试播放');
 f.ctx.startTimelineVideo(f.button);f.ctx.releaseTimelineMedia();f.ctx.startTimelineVideo(f.button);
 f.plays[1].reject(new Error('stale'));await tick();assert.equal(f.video.hidden,false);assert.equal(f.video.src,f.button.dataset.videoSrc);
 f.plays[2].resolve();await tick();
});
test('browser sound restrictions leave separate controls available for a user gesture',async()=>{
 const f=fixture();f.state.settings.video_default_muted=false;f.ctx.startTimelineVideo(f.button);
 const error=new Error('gesture required');error.name='NotAllowedError';f.plays[0].reject(error);await tick();
 assert.equal(f.video.hidden,false);assert.equal(f.button.hidden,true);assert.equal(f.notices.length,1);
});
test('fullscreen toggles preserve source, progress, playback and sound; release restores scrolling',()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);
 assert.equal(f.shell.classList.contains('video-expanded'),true);assert.equal(f.document.body.style.overflow,'hidden');
 f.video.currentTime=27;f.video.muted=false;f.video.loop=true;
 f.ctx.toggleVideoFullscreen(f.video);
 assert.equal(f.shell.classList.contains('video-expanded'),false);assert.equal(f.document.body.style.overflow,'auto');
 assert.equal(f.video.currentTime,27);assert.equal(f.video.paused,false);assert.equal(f.video.muted,false);assert.equal(f.video.loop,true);assert.equal(f.video.src,f.button.dataset.videoSrc);
 f.ctx.toggleVideoFullscreen(f.video);assert.equal(f.video.currentTime,27);assert.equal(f.plays.length,1);
 f.ctx.releaseTimelineMedia();assert.equal(f.document.body.style.overflow,'auto');assert.equal(f.controls.hidden,true);
});
test('separate play, mute and seek controls do not toggle fullscreen',()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);
 const click=action=>f.postEvents.click({preventDefault(){},target:{closest:q=>q==='[data-video-src]'?null:q==='.video-shell'?f.shell:{dataset:{videoAction:action}}}});
 click('play');assert.equal(f.video.paused,true);
 click('mute');assert.equal(f.video.muted,false);
 f.postEvents.input({target:{matches:()=>true,closest:()=>f.shell,value:350}});
 assert.equal(f.video.currentTime,35);assert.equal(f.shell.classList.contains('video-expanded'),true);
 click('play');assert.equal(f.video.paused,false);assert.equal(f.plays.length,2);
});
test('rejected native fullscreen keeps the page-sized fallback, Escape restores inline',async()=>{
 const f=fixture();f.document.fullscreenEnabled=true;f.shell.requestFullscreen=()=>Promise.reject(new Error('denied'));
 f.ctx.startTimelineVideo(f.button);await tick();
 assert.equal(f.shell.classList.contains('video-expanded'),true);
 f.documentEvents.keydown({key:'Escape',preventDefault(){}});
 assert.equal(f.shell.classList.contains('video-expanded'),false);assert.equal(f.video.paused,false);
});
test('late native fullscreen entry is dismissed after the player was collapsed',async()=>{
 const f=fixture();let finish;
 f.document.fullscreenEnabled=true;f.shell.requestFullscreen=()=>new Promise(resolve=>finish=resolve);
 f.ctx.startTimelineVideo(f.button);f.ctx.collapseExpandedVideo();
 f.document.fullscreenElement=f.shell;finish();await tick();
 assert.equal(f.document.fullscreenElement,null);assert.equal(f.document.body.style.overflow,'auto');
});
test('native fullscreen exit restores inline without resetting the video',()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);f.video.currentTime=42;
 f.document.fullscreenElement=f.shell;f.documentEvents.fullscreenchange();
 f.document.fullscreenElement=null;f.documentEvents.fullscreenchange();
 assert.equal(f.shell.classList.contains('video-expanded'),false);assert.equal(f.video.currentTime,42);
 assert.equal(f.video.src,f.button.dataset.videoSrc);assert.equal(f.video.paused,false);
});

test('single click keeps an expanded video open',()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);
 f.postEvents.click({target:f.video});
 assert.equal(f.shell.classList.contains('video-expanded'),true);
 assert.equal(f.document.body.style.overflow,'hidden');
});
test('next video pauses the previous player and retains an already loaded source',async()=>{
 const f=fixture(),next=fixture();f.ctx.startTimelineVideo(f.button);
 next.video.src='/media/99/video';next.video.currentTime=12;
 f.ctx.nextTimelineMedia=async()=>next.shell;
 await f.ctx.nextExpandedVideo();
 assert.equal(f.video.paused,true);assert.equal(f.shell.classList.contains('video-expanded'),false);
 assert.equal(next.shell.classList.contains('video-expanded'),true);assert.equal(next.video.paused,false);
 assert.equal(next.video.src,'/media/99/video');assert.equal(next.video.currentTime,12);
 assert.equal(f.document.body.style.overflow,'hidden');
});
test('leaving the player while finding the next video cancels the switch',async()=>{
 const f=fixture(),next=fixture();f.ctx.startTimelineVideo(f.button);let finish,calls=0;
 f.ctx.nextTimelineMedia=()=>{calls++;return new Promise(resolve=>finish=resolve)};
 const pending=f.ctx.nextExpandedVideo();await f.ctx.nextExpandedVideo();
 assert.equal(calls,1);
 f.ctx.collapseExpandedVideo();finish(next.shell);await pending;
 assert.equal(next.video.src,'');assert.equal(f.document.body.style.overflow,'auto');
 assert.equal(f.shell.classList.contains('video-expanded'),false);
});
