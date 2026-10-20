const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const state={settings:{}},plays=[],notices=[],events={};
 const strong={textContent:''},small={textContent:''};
 const video={dataset:{},hidden:true,isConnected:true,muted:false,src:'',pauses:0,loads:0,getAttribute(k){return k==='src'?this.src:null},removeAttribute(k){if(k==='src')this.src=''},pause(){this.pauses++},load(){this.loads++},play(){return new Promise((resolve,reject)=>plays.push({resolve,reject,muted:this.muted}))},closest(){return shell}};
 const button={dataset:{videoSrc:'/media/100/asset?v=hash'},hidden:false,closest(){return shell},querySelector(q){return q==='strong'?strong:small}};
 const shell={querySelector(q){return q==='video'?video:button}};
 const ctx=vm.createContext({state,document:{querySelectorAll:()=>video.src?[video]:[]},window:{addEventListener:(k,fn)=>events[k]=fn},escape:v=>String(v).replaceAll('&','&amp;').replaceAll('"','&quot;'),icon:()=>'',toast:m=>notices.push(m)});
 vm.runInContext(source.slice(source.indexOf('function defaultVideoMuted()'),source.indexOf('function displayPostText(')),ctx);
 return {ctx,state,video,button,plays,strong,small,events,notices};
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
test('failed playback offers retry; stale rejected playback cannot reset the new attempt',async()=>{
 const f=fixture();f.ctx.startTimelineVideo(f.button);f.plays[0].reject(new Error('network'));await tick();
 assert.equal(f.video.src,'');assert.equal(f.button.hidden,false);assert.equal(f.strong.textContent,'重试播放');
 f.ctx.startTimelineVideo(f.button);f.ctx.releaseTimelineMedia();f.ctx.startTimelineVideo(f.button);
 f.plays[1].reject(new Error('stale'));await tick();assert.equal(f.video.hidden,false);assert.equal(f.video.src,f.button.dataset.videoSrc);
 f.plays[2].resolve();await tick();
});
test('browser sound restrictions leave native controls available for a user gesture',async()=>{
 const f=fixture();f.state.settings.video_default_muted=false;f.ctx.startTimelineVideo(f.button);
 const error=new Error('gesture required');error.name='NotAllowedError';f.plays[0].reject(error);await tick();
 assert.equal(f.video.hidden,false);assert.equal(f.button.hidden,true);assert.equal(f.notices.length,1);
});
