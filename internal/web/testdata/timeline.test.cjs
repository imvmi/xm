// Run with: node --test internal/web/testdata/timeline.test.cjs
const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const notices=[],nodes=new Map(), requests=[], observer={disconnect(){this.targets=[]},observe(target){this.observed++;this.targets.push(target)},observed:0,targets:[]};
 const $=id=>{if(!nodes.has(id))nodes.set(id,{value:'',textContent:'',hidden:false,html:'',children:[],attrs:{},addEventListener(){},setAttribute(k,v){this.attrs[k]=v},replaceChildren(){this.html='';this.children=[]},insertAdjacentHTML(_,html){this.html+=html;this.children.push(...[...html.matchAll(/<article class="post" data-post-id="([^"]+)"/g)].map(m=>({id:m[1]})))},querySelector(){return $('heading')}});return nodes.get(id)};
 const state={authenticated:true,view:'user',target:'alice',timelineTab:'posts',postRequest:0,postIDs:new Set(),postsLoading:false,postsError:false,hasMore:false,offset:0,total:0};
 const ctx=vm.createContext({state,$,window:{IntersectionObserver:true},IntersectionObserver:function(cb){observer.callback=cb;return observer},URLSearchParams,clearTimeout,searchTimer:null,number:String,escape:String,avatar:()=>'',userAvatar:()=>'',icon:()=>'',date:String,mediaHTML:()=>'',quoteHTML:()=>'',releaseTimelineMedia:()=>{},toast:message=>notices.push(message),api:query=>new Promise((resolve,reject)=>requests.push({query,resolve,reject}))});
 vm.runInContext(source.slice(source.indexOf('function displayPostText('),source.indexOf("$('posts').addEventListener")),ctx);
 const page=(ids,total)=>({total,items:ids.map(id=>({id,username:'alice',text:`post-${id}`,media:[]}))});
 return {ctx,state,$,requests,observer,page,notices};
}
test('append preserves existing DOM; concurrent loads and duplicate IDs are suppressed',async()=>{
 const f=fixture();let pending=f.ctx.refreshPosts();
 f.requests[0].resolve(f.page(['3','2'],4));await pending;
 f.$('posts').html+='EXPANDED_OR_PLAYING';
 pending=f.ctx.loadMorePosts();assert.equal(f.ctx.loadMorePosts(),pending);
 assert.equal(f.requests.length,2);assert.match(f.requests[1].query,/offset=2/);
 f.requests[1].resolve(f.page(['2','1'],4));await pending;
 assert.match(f.$('posts').html,/EXPANDED_OR_PLAYING/);
 assert.equal((f.$('posts').html.match(/post-2/g)||[]).length,1);
 assert.equal(f.state.postIDs.size,3);assert.equal(f.state.hasMore,false);
 assert.equal(f.$('load-more').hidden,true);
});
test('old requests cannot overwrite new tab, user, or search results',async()=>{
 const f=fixture();const old=f.ctx.refreshPosts();
 f.state.timelineTab='replies';f.state.target='bob';f.$('search').value='new';
 const current=f.ctx.refreshPosts();
 f.requests[1].resolve(f.page(['7'],1));await current;
 f.requests[0].resolve(f.page(['9'],100));await old;
 assert.match(f.requests[1].query,/target=bob/);assert.match(f.requests[1].query,/section=replies/);
 assert.match(f.requests[1].query,/q=new/);assert.equal(f.state.total,1);
 assert.match(f.$('posts').html,/post-7/);assert.doesNotMatch(f.$('posts').html,/post-9/);
});
test('failure keeps loaded posts; no automatic retry loop; retry uses the same offset',async()=>{
 const f=fixture();let pending=f.ctx.refreshPosts();f.requests[0].resolve(f.page(['2'],2));await pending;
 const html=f.$('posts').html;
 pending=f.ctx.loadMorePosts();f.requests[1].reject(new Error('offline'));await pending;
 assert.equal(f.$('posts').html,html);assert.equal(f.state.offset,1);
 assert.equal(f.$('load-more').textContent,'重试加载');assert.equal(f.$('load-more').hidden,false);
 f.observer.callback([{isIntersecting:true}]);assert.equal(f.requests.length,2);
 pending=f.ctx.loadMorePosts();assert.match(f.requests[2].query,/offset=1/);
 f.requests[2].resolve(f.page(['1'],2));await pending;
 assert.equal(f.state.postsError,false);assert.equal(f.state.postIDs.size,2);
});
test('an empty final page stops loading even when the count changed',async()=>{
 const f=fixture();const pending=f.ctx.refreshPosts();f.requests[0].resolve(f.page([],10));await pending;
 assert.equal(f.state.hasMore,false);assert.equal(f.$('post-empty').hidden,false);
 await f.ctx.loadMorePosts();assert.equal(f.requests.length,1);
});
test('home uses the global timeline and continues by cursor without recounting',async()=>{
 const f=fixture();f.state.view='home';f.state.target='';f.state.timelineTab='';
 let pending=f.ctx.refreshPosts();
 f.requests[0].resolve({...f.page(['100','99'],3),next_cursor:'99',has_more:true});await pending;
 pending=f.ctx.loadMorePosts();
 assert.match(f.requests[1].query,/before=99/);assert.match(f.requests[1].query,/count=0/);assert.doesNotMatch(f.requests[1].query,/offset=/);
 f.requests[1].resolve({...f.page(['98'],-1),next_cursor:'98',has_more:false});await pending;
 assert.equal(f.state.total,3);assert.equal(f.state.postIDs.size,3);assert.equal(f.state.hasMore,false);
});
test('home and account feeds prefetch at the third-last post, then move the trigger',async()=>{
 for(const view of ['home','user']){
  const f=fixture();f.state.view=view;
  let pending=f.ctx.refreshPosts();
  f.requests[0].resolve({...f.page(['10','9','8','7','6'],10),has_more:true});await pending;
  const old=f.$('posts').children[2];
  assert.equal(f.observer.targets[0],old);
  assert.equal(f.observer.targets[1],f.$('timeline-loader'));
  f.observer.callback([{target:old,isIntersecting:true},{target:f.$('timeline-loader'),isIntersecting:true}]);
  assert.equal(f.requests.length,2);
  f.observer.callback([{target:old,isIntersecting:true}]);
  assert.equal(f.requests.length,2);
  f.requests[1].resolve({...f.page(['5','4','3'],10),has_more:true});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.observer.targets[0],f.$('posts').children[5]);
  f.observer.callback([{target:old,isIntersecting:true}]);
  assert.equal(f.requests.length,2,'old trigger cannot request another page');
  // Removing a bookmark shifts the trigger to the new third-last post.
  f.$('posts').children.pop();f.ctx.updateTimelineLoader();
  assert.equal(f.observer.targets[0],f.$('posts').children[4]);
 }
});
test('short pages prefetch from the first post; completed and failed pages stop observing',async()=>{
 const f=fixture();let pending=f.ctx.refreshPosts();
 f.requests[0].resolve({...f.page(['2','1'],3),has_more:true});await pending;
 const first=f.$('posts').children[0];
 assert.equal(f.observer.targets[0],first);
 pending=f.ctx.loadMorePosts();f.requests[1].reject(Error('offline'));await pending;
 assert.equal(f.observer.targets.length,0);
 f.observer.callback([{target:first,isIntersecting:true}]);assert.equal(f.requests.length,2);
 pending=f.ctx.loadMorePosts();f.requests[2].resolve({...f.page(['0'],3),has_more:false});await pending;
 assert.equal(f.observer.targets.length,0);
});
test('bookmark feeds carry the account scope and composite cursor',async()=>{
 const f=fixture();f.state.timelineTab='bookmarks';
 let pending=f.ctx.refreshPosts();
 assert.match(f.requests[0].query,/section=bookmarks/);assert.match(f.requests[0].query,/target=alice/);
 f.requests[0].resolve({...f.page(['9'],2),next_cursor:'b:123:9',has_more:true});await pending;
 pending=f.ctx.loadMorePosts();
 assert.match(f.requests[1].query,/before=b%3A123%3A9/);
 f.requests[1].resolve({...f.page(['2'],-1),has_more:false});await pending;
 assert.equal(f.state.postIDs.size,2);
});
test('an old page response cannot undo a successful bookmark change',async()=>{
 const f=fixture();const pending=f.ctx.refreshPosts();
 vm.runInContext(`bookmarkOverrides.set('9',{bookmarked:true,bookmarked_at:123,revision:++bookmarkRevision})`,f.ctx);
 f.requests[0].resolve(f.page(['9'],1));await pending;
 assert.match(f.$('posts').html,/data-bookmark-id="9" aria-pressed="true"/);
});
test('an in-flight bookmark page is refreshed after a collection change',async()=>{
 const f=fixture();f.state.timelineTab='bookmarks';const pending=f.ctx.refreshPosts();
 vm.runInContext(`bookmarkOverrides.set('9',{bookmarked:false,bookmarked_at:0,revision:++bookmarkRevision})`,f.ctx);
 f.requests[0].resolve(f.page(['9'],1));await new Promise(resolve=>setImmediate(resolve));
 assert.equal(f.requests.length,2);
 f.requests[1].resolve(f.page([],0));await pending;
 assert.equal(f.state.postIDs.size,0);assert.equal(f.state.total,0);
});
test('the author handle opens the source post and replies retain context access',()=>{
 const f=fixture();
 const html=f.ctx.postHTML({id:'42',username:'Vickiehavefun',name:'Vickie',created_at:1,text:'reply',reply_to:'41',media:[]});
 assert.match(html,/<a class="timeline-handle" href="https:\/\/x\.com\/i\/status\/42"[^>]*>@Vickiehavefun<\/a>/);
 assert.doesNotMatch(html,/>\s*回复\s*</);
 assert.match(html,/data-reply-context="42"/);
 assert.match(html,/https:\/\/x\.com\/i\/status\/41/);
 assert.doesNotMatch(html,/post-footer/);
});
test('media links do not leave trailing blank lines; meaningful paragraphs remain intact',()=>{
 const f=fixture();
 for(const text of ['https://t.co/photo','\n\nhttps://t.co/photo',' \n https://t.co/photo \n']){
  assert.equal(f.ctx.displayPostText({text,media:[{}]}),'');
 }
 assert.equal(f.ctx.displayPostText({text:'正文\n\n https://t.co/photo',media:[{}]}),'正文');
 assert.equal(f.ctx.displayPostText({text:'正文\nhttps://t.co/one\nhttps://t.co/two',media:[{},{}]}),'正文');
 assert.equal(f.ctx.displayPostText({text:'第一行\n第二行\n\n下一段\n\nhttps://t.co/photo',media:[{}]}),'第一行\n第二行\n\n下一段');
 assert.equal(f.ctx.displayPostText({text:'外部链接\nhttps://t.co/article',media:[]}),'外部链接\nhttps://t.co/article');
 const html=f.ctx.quoteHTML({quote_id:'77',quote:{id:'77',username:'alice',text:'引用正文\n\n https://t.co/photo',media:[{}]}});
 assert.match(html,/<div class="post-text quote-text">引用正文<\/div>/);
});
test('an archived quote links its handle without a separate source footer',()=>{
 const f=fixture();
 const html=f.ctx.quoteHTML({quote_id:'77',quote:{id:'77',username:'fishball173',name:'fishball',created_at:1,text:'quoted',media:[]}});
 assert.match(html,/<a class="quote-handle" href="https:\/\/x\.com\/i\/status\/77"[^>]*>@fishball173<\/a>/);
 assert.doesNotMatch(html,/已归档的引用推文|quote-source|data-bookmark-id/);
 assert.match(html,/data-quote-post="77"/);
});

function profileHTML(user){
 const nodes=new Map(),$=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',innerHTML:''});return nodes.get(id)};
 const state={users:[user],target:user.username};
 const ctx=vm.createContext({state,$,document:{title:''},number:String,userStat:(u,key)=>u.stats_pending?'—':String(u[key]),escape:String,date:value=>'DATE:'+value,icon:name=>`<svg data-icon="${name}"></svg>`,avatar:()=>'<span class="avatar"></span>',handle:u=>u.current_username||u.username});
 vm.runInContext(source.slice(source.indexOf('function renderProfile()'),source.indexOf('const timelineTabs=')),ctx);
 ctx.renderProfile();
 return $('profile-header').innerHTML;
}

test('profile handle opens X while archive status occupies the former action position',()=>{
 const html=profileHTML({username:'zlswwww',name:'zlswwww',posts:0,media:0,monitored:true,enabled:false});
 assert.match(html,/<a href="https:\/\/x\.com\/zlswwww" class="user-handle profile-x-link"[^>]*>@zlswwww<\/a>/);
 assert.match(html,/<div class="profile-actions">[\s\S]*<div class="profile-meta profile-archive-status">[\s\S]*尚未归档[\s\S]*<\/div><\/div><h2>/);
 assert.doesNotMatch(html,/>在 X 查看/);
 const archived=profileHTML({username:'alice',name:'Alice',posts:1,media:0,last_saved:'2026-10-18T01:00:00Z'});
 assert.match(archived,/profile-archive-status[\s\S]*最近归档于 DATE:2026-10-18T01:00:00Z/);
});

test('media navigation skips empty pages and shares an in-flight timeline load',async()=>{
 const f=fixture();let pending=f.ctx.refreshPosts();f.requests[0].resolve({...f.page(['9'],30),has_more:true});await pending;
 const current={isConnected:true},next={isConnected:true},media=[current];f.$('posts').querySelectorAll=()=>media;
 pending=f.ctx.loadMorePosts();const navigation=f.ctx.nextTimelineMedia(current,'video',()=>true);
 assert.equal(f.requests.length,2);
 f.requests[1].resolve({...f.page(['8'],30),has_more:true});await pending;await new Promise(setImmediate);
 assert.equal(f.requests.length,3);
 media.push(next);f.requests[2].resolve({...f.page(['7'],30),has_more:true});
 assert.equal(await navigation,next);
});
test('closing a viewer or changing timeline cancels pending media navigation',async()=>{
 for(const cancel of ['viewer','timeline']){
  const f=fixture();let pending=f.ctx.refreshPosts();f.requests[0].resolve({...f.page(['9'],30),has_more:true});await pending;
  const current={isConnected:true},next={isConnected:true},media=[current];f.$('posts').querySelectorAll=()=>media;
  let active=true;pending=f.ctx.nextTimelineMedia(current,'video',()=>active);
  if(cancel==='viewer')active=false;else f.state.postRequest++;
  media.push(next);f.requests[1].resolve({...f.page(['8'],30),has_more:true});
  assert.equal(await pending,null);assert.equal(f.requests.length,2);
 }
});

test('closing a speculative consumer discards its page without advancing the cursor',async()=>{
 const f=fixture();let p=f.ctx.refreshPosts();f.requests[0].resolve({...f.page(['5'],10),has_more:true,next_cursor:'5'});await p;
 let active=true;p=f.ctx.loadMorePosts({background:true,isCurrent:()=>active});active=false;
 f.requests[1].resolve({...f.page(['4'],10),has_more:true,next_cursor:'4'});await p;
 assert.equal(f.state.offset,1);assert.equal(f.state.cursor,'5');assert.equal(f.state.postsLoading,false);assert.equal(f.state.postIDs.has('4'),false);
});
test('foreground navigation can promote and reuse an in-flight speculative page',async()=>{
 const f=fixture();let p=f.ctx.refreshPosts();f.requests[0].resolve({...f.page(['5'],10),has_more:true,next_cursor:'5'});await p;
 let active=true;p=f.ctx.loadMorePosts({background:true,isCurrent:()=>active});const foreground=f.ctx.loadMorePosts();active=false;
 assert.equal(p,foreground);assert.equal(f.requests.length,2);
 f.requests[1].resolve({...f.page(['4'],10),has_more:true,next_cursor:'4'});await p;
 assert.equal(f.state.offset,2);assert.equal(f.state.cursor,'4');assert.equal(f.state.postIDs.has('4'),true);
});

test('speculative failure stays quiet and foreground navigation can retry the same cursor',async()=>{
 const f=fixture();let p=f.ctx.refreshPosts();f.requests[0].resolve({...f.page(['5'],10),has_more:true,next_cursor:'5'});await p;
 p=f.ctx.loadMorePosts({background:true,isCurrent:()=>true});f.requests[1].reject(new Error('offline'));await p;
 assert.equal(f.notices.length,0);assert.equal(f.state.cursor,'5');assert.equal(f.state.postsError,true);
 p=f.ctx.loadMorePosts();assert.match(f.requests[2].query,/before=5/);f.requests[2].resolve({...f.page(['4'],10),has_more:true,next_cursor:'4'});await p;
 assert.equal(f.state.postsError,false);assert.equal(f.state.cursor,'4');
});

test('first pages skip full counts and show loaded rows until the last cursor page',async()=>{
 for(const tab of ['', 'posts', 'replies', 'media', 'bookmarks']){
  const f=fixture();f.state.timelineTab=tab;f.$('search').value='quoted';f.$('kind-filter').value='video';
  let pending=f.ctx.refreshPosts();
  const query=new URLSearchParams(f.requests[0].query.split('?')[1]);
  assert.equal(query.get('count'),'0');assert.equal(query.get('section'),tab);
  assert.equal(query.get('q'),'quoted');assert.equal(query.get('kind'),'video');
  f.requests[0].resolve({...f.page(['3','2'],-1),next_cursor:'2',has_more:true});await pending;
  assert.equal(f.state.total,-1);assert.equal(f.$('result-count').textContent,'已加载 2');
  assert.equal(f.$('load-status').textContent,'已显示 2 条');
  pending=f.ctx.loadMorePosts();f.requests[1].reject(new Error('offline'));await pending;
  assert.equal(f.$('result-count').textContent,'已加载 2');
  pending=f.ctx.loadMorePosts();
  assert.match(f.requests[2].query,/before=2/);
  f.requests[2].resolve({...f.page(['2','1'],-1),next_cursor:'1',has_more:false});await pending;
  assert.equal(f.state.total,3);assert.equal(f.$('result-count').textContent,'3');
  assert.equal(f.$('load-status').textContent,'已显示 3 条，共 3 条 · 已全部加载');
 }
});
test('an uncounted empty first page displays zero and stops pagination',async()=>{
 const f=fixture(),pending=f.ctx.refreshPosts();
 f.requests[0].resolve({...f.page([],-1),has_more:false});await pending;
 assert.equal(f.state.total,0);assert.equal(f.$('result-count').textContent,'0');
 assert.equal(f.state.hasMore,false);assert.equal(f.$('post-empty').hidden,false);
});
test('initial navigation gets the SQLite connection before historical overview counts',async()=>{
 const calls=[],nodes=new Map();let finishNavigation;
 const $=id=>{if(!nodes.has(id))nodes.set(id,{});return nodes.get(id)};
 const ctx=vm.createContext({$,state:{},applyArchiveTheme:()=>{},api:async route=>{calls.push(route);return {}},navigate:()=>{calls.push('navigate');return new Promise(resolve=>finishNavigation=resolve)},refreshOverview:async()=>{calls.push('overview')},toast:()=>{}});
 vm.runInContext(source.slice(source.indexOf('async function enter()'),source.indexOf("$('login-form').addEventListener")),ctx);
 const pending=ctx.enter();await new Promise(setImmediate);
 assert.deepEqual(calls,['/settings','navigate']);
 finishNavigation();await pending;assert.deepEqual(calls,['/settings','navigate','overview']);
});
