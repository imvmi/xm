// Run with: node --test internal/web/testdata/timeline.test.cjs
const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
function fixture(){
 const nodes=new Map(), requests=[], observer={disconnect(){},observe(){this.observed++},observed:0};
 const $=id=>{if(!nodes.has(id))nodes.set(id,{value:'',textContent:'',hidden:false,html:'',attrs:{},addEventListener(){},setAttribute(k,v){this.attrs[k]=v},replaceChildren(){this.html=''},insertAdjacentHTML(_,html){this.html+=html},querySelector(){return $('heading')}});return nodes.get(id)};
 const state={authenticated:true,view:'user',target:'alice',timelineTab:'posts',postRequest:0,postIDs:new Set(),postsLoading:false,postsError:false,hasMore:false,offset:0,total:0};
 const ctx=vm.createContext({state,$,window:{IntersectionObserver:true},IntersectionObserver:function(cb){observer.callback=cb;return observer},URLSearchParams,clearTimeout,searchTimer:null,number:String,escape:String,avatar:()=>'',icon:()=>'',date:String,mediaHTML:()=>'',quoteHTML:()=>'',toast:()=>{},api:query=>new Promise((resolve,reject)=>requests.push({query,resolve,reject}))});
 vm.runInContext(source.slice(source.indexOf('function quoteHTML('),source.indexOf("$('posts').addEventListener")),ctx);
 const page=(ids,total)=>({total,items:ids.map(id=>({id,username:'alice',text:`post-${id}`,media:[]}))});
 return {ctx,state,$,requests,observer,page};
}
test('append preserves existing DOM; concurrent loads and duplicate IDs are suppressed',async()=>{
 const f=fixture();let pending=f.ctx.refreshPosts();
 f.requests[0].resolve(f.page(['3','2'],4));await pending;
 f.$('posts').html+='EXPANDED_OR_PLAYING';
 pending=f.ctx.loadMorePosts();await f.ctx.loadMorePosts();
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
test('the author handle opens the source post without redundant reply controls',()=>{
 const f=fixture();
 const html=f.ctx.postHTML({id:'42',username:'Vickiehavefun',name:'Vickie',created_at:1,text:'reply',reply_to:'41',media:[]});
 assert.match(html,/<a class="timeline-handle" href="https:\/\/x\.com\/i\/status\/42"[^>]*>@Vickiehavefun<\/a>/);
 assert.doesNotMatch(html,/>\s*回复\s*</);
 assert.doesNotMatch(html,/查看原推文|post-footer/);
});
test('an archived quote links its handle without a separate source footer',()=>{
 const f=fixture();
 const html=f.ctx.quoteHTML({quote_id:'77',quote:{id:'77',username:'fishball173',name:'fishball',created_at:1,text:'quoted',media:[]}});
 assert.match(html,/<a class="quote-handle" href="https:\/\/x\.com\/i\/status\/77"[^>]*>@fishball173<\/a>/);
 assert.doesNotMatch(html,/已归档的引用推文|quote-source/);
});
