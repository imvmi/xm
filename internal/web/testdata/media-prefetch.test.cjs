const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/media-prefetch.js'),'utf8');
const tick=()=>new Promise(resolve=>setImmediate(resolve));
function fixture(){
 const events={},images=[],calls=[];
 class Img{constructor(){this.src='';images.push(this)}removeAttribute(){this.src=''}decode(){return Promise.resolve()}}
 const state={settings:{},postRequest:1,hasMore:true,postsError:false,offset:10};
 const nodes={'image-dialog':{open:true},'large-image':{complete:true,naturalWidth:1,addEventListener(){}}};
 let items=Array.from({length:6},()=>({isConnected:true}));
 nodes.posts={querySelectorAll:()=>items};
 const ctx=vm.createContext({state,$:id=>nodes[id],document:{hidden:false,addEventListener:(k,v)=>events[k]=v},Image:Img,
  imageGallery:['a','b','c','d','e'],imageIndex:0,gallerySession:1,galleryTimeline:true,refreshGalleryNeighbors(){},expandedVideoShell:null,videoFullscreenRequest:1,
  loadMorePosts:async options=>{calls.push(options);state.offset+=10;}});
 vm.runInContext(source,ctx);
 return {ctx,state,nodes,calls,images,events,get items(){return items},set items(v){items=v}};
}
test('media count triggers early pagination and media-sparse scans stop after two pages',async()=>{
 const f=fixture();await f.ctx.prefetchMediaPages(()=>f.items[0],'images',3,()=>true);assert.equal(f.calls.length,0);
 await f.ctx.prefetchMediaPages(()=>f.items[2],'images',3,()=>true);assert.equal(f.calls.length,2);
 assert.ok(f.calls.every(c=>c.background&&c.isCurrent()));
 f.state.hasMore=false;await f.ctx.prefetchMediaPages(()=>f.items[5],'images',3,()=>true);assert.equal(f.calls.length,2);
});
test('navigation consumers share a scan and close/filter changes stop further requests',async()=>{
 for(const change of ['close','filter']){
  const f=fixture();let resolve,active=true;
  f.ctx.loadMorePosts=options=>{f.calls.push(options);return new Promise(r=>resolve=()=>{f.state.offset++;r()})};
  const first=f.ctx.prefetchMediaPages(()=>f.items[5],'images',3,()=>active);
  const second=f.ctx.prefetchMediaPages(()=>f.items[4],'images',3,()=>active);
  assert.equal(f.calls.length,1);assert.equal(first,second);
  if(change==='close')active=false;else f.state.postRequest++;
  assert.equal(f.calls[0].isCurrent(),false);resolve();await first;assert.equal(f.calls.length,1);
 }
});
test('disabled, hidden, failed, and sufficiently buffered views do not prefetch',async()=>{
 const f=fixture();for(const mode of ['off','hidden','failed']){
  f.state.settings.media_preload_mode=mode==='off'?'off':'standard';f.ctx.document.hidden=mode==='hidden';f.state.postsError=mode==='failed';
  await f.ctx.prefetchMediaPages(()=>f.items[5],'images',3,()=>true);
 }
 assert.equal(f.calls.length,0);
});
test('images wait for the current image and retain only two upcoming URLs',()=>{
 const f=fixture();f.nodes['large-image'].complete=false;f.ctx.warmGalleryImages();assert.equal(f.images.length,0);
 f.nodes['large-image'].complete=true;f.ctx.warmGalleryImages();assert.deepEqual(f.images.map(i=>i.src),['b','c']);
 f.ctx.imageIndex=1;f.ctx.warmGalleryImages();assert.deepEqual(f.images.map(i=>i.src),['','c','d']);
 f.ctx.clearGalleryPrefetch();assert.ok(f.images.every(i=>!i.src));
});
function videoShell(src){
 const video={dataset:{},src:'',preload:'none',paused:true,ended:false,readyState:0,loads:0,isConnected:true,getAttribute(){return this.src},removeAttribute(){this.src=''},load(){this.loads++},pause(){this.paused=true}};
 return {video,isConnected:true,querySelector:s=>s==='video'?video:{dataset:{videoSrc:src}}};
}
test('standard/enhanced modes preload only the next video without playing it',async()=>{
 for(const mode of ['standard','enhanced']){
  const f=fixture(),shells=['one','two','three'].map(videoShell);f.items=shells;f.state.hasMore=false;f.state.settings.media_preload_mode=mode;
  f.ctx.expandedVideoShell=shells[0];Object.assign(shells[0].video,{paused:false,readyState:4});
  f.ctx.requestVideoPrefetch();await tick();
  assert.equal(shells[1].video.preload,mode==='standard'?'metadata':'auto');assert.equal(shells[1].video.src,'two');assert.equal(shells[1].video.paused,true);assert.equal(shells[2].video.src,'');
  f.ctx.requestVideoPrefetch();assert.equal(shells[1].video.loads,1);
  // Stalls release speculative downloads, then allow resuming once stable.
  shells[0].video.readyState=1;f.ctx.requestVideoPrefetch();assert.equal(shells[1].video.src,'');
  shells[0].video.readyState=4;f.ctx.requestVideoPrefetch();assert.equal(shells[1].video.src,'two');
  const loads=shells[1].video.loads;f.ctx.claimVideoPrefetch(shells[1].video);f.ctx.clearVideoPrefetch();
  assert.equal(shells[1].video.src,'two');assert.equal(shells[1].video.loads,loads);assert.equal(shells[1].video.dataset.prefetch,undefined);
 }
});
test('failed video speculation does not repeatedly retry or touch the playing video',async()=>{
 const f=fixture(),shells=['one','two'].map(videoShell);f.items=shells;f.state.hasMore=false;f.ctx.expandedVideoShell=shells[0];Object.assign(shells[0].video,{src:'one',paused:false,readyState:4});
 f.ctx.requestVideoPrefetch();await tick();f.ctx.clearVideoPrefetch(null,true);const loads=shells[1].video.loads;
 f.ctx.requestVideoPrefetch();assert.equal(shells[1].video.src,'');assert.equal(shells[1].video.loads,loads);assert.equal(shells[0].video.paused,false);assert.equal(shells[0].video.src,'one');
});
