'use strict';
const docsEscape=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
(async()=>{try{
 const response=await fetch('/docs/api/openapi.json',{credentials:'omit'});if(!response.ok)throw Error('接口文档暂时不可用，请稍后刷新。');const spec=await response.json();
 document.getElementById('api-reference').innerHTML=Object.entries(spec.paths).flatMap(([path,methods])=>Object.entries(methods).map(([method,op])=>{
  const body=op.requestBody?.content?.['application/json'],parameters=op.parameters||[];
  return `<details><summary><span class="method">${docsEscape(method.toUpperCase())}</span> <code>${docsEscape(path)}</code></summary><p class="endpoint-description">${docsEscape(op.summary)} · ${docsEscape(op['x-scope']||'有效凭据')}</p>${parameters.length?`<div class="table-wrap"><table><thead><tr><th>参数</th><th>位置</th><th>约束</th></tr></thead><tbody>${parameters.map(p=>`<tr><td><code>${docsEscape(p.name)}</code>${p.required?' *':''}</td><td>${docsEscape(p.in)}</td><td>${docsEscape(p.description||JSON.stringify(p.schema))}</td></tr>`).join('')}</tbody></table></div>`:''}${body?`<p>JSON 请求示例</p><pre><code>${docsEscape(JSON.stringify(body.example,null,2))}</code></pre><p>请求结构</p><pre><code>${docsEscape(JSON.stringify(body.schema,null,2))}</code></pre>`:''}<details><summary>响应结构</summary><pre><code>${docsEscape(JSON.stringify(op.responses,null,2))}</code></pre></details></details>`;
 })).join('')+`<details><summary>共享数据结构</summary><pre><code>${docsEscape(JSON.stringify(spec.components.schemas,null,2))}</code></pre></details>`;
}catch(e){document.getElementById('docs-error').textContent=e.message;}})();
