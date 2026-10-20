const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

const source=fs.readFileSync(path.join(__dirname,'../static/app.js'),'utf8');
const start=source.indexOf("for(const header of document.querySelectorAll('[data-timeline-top]')){");
assert.notEqual(start,-1);
const handler=source.slice(start,source.indexOf('\n\n',start));

for(const reduced of [false,true]){
 test(`timeline header scrolls without navigation; reduced motion=${reduced}`,()=>{
  const listeners=[],calls=[];
  const headers=[0,1].map(()=>({addEventListener(type,fn){assert.equal(type,'click');listeners.push(fn);}}));
  vm.runInNewContext(handler,{
   document:{querySelectorAll:()=>headers},
   window:{matchMedia:query=>{assert.equal(query,'(prefers-reduced-motion: reduce)');return {matches:reduced};},scrollTo:options=>calls.push(options)}
  });
  assert.equal(listeners.length,2);
  for(const click of listeners){
   click({target:{closest:()=>null}});
   assert.equal(calls.at(-1).top,0);
   assert.equal(calls.at(-1).behavior,reduced?'instant':'smooth');
   const count=calls.length;
   // Refresh buttons (including their SVG children) and back links stay independent.
   click({target:{closest:selector=>{assert.equal(selector,'a,button:not(.timeline-top-button)');return {};}}});
   assert.equal(calls.length,count);
  }
 });
}
