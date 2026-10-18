package archive

import (
	"encoding/json"
	"net/url"
)

const checkpointPageBudget = 5

func requestCursor(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	var variables map[string]any
	if json.Unmarshal([]byte(u.Query().Get("variables")), &variables) != nil {
		return ""
	}
	return str(variables["cursor"])
}

func checkpointFailure() error {
	return fail("checkpoint", "X 分页断点失效或未能应用，已暂停以避免重复读取；请重置断点后重试")
}

// Keep the live page's query ID, feature flags and authentication. Substitute
// only its first timeline cursor, before fetch/XHR sends the request. The Go
// response reader checks the actual request URL and fails closed if the page
// uses another transport or ignores the substitution.
func resumeScript(progress CollectionProgress) string {
	data, _ := json.Marshal(progress)
	return `(()=>{
const p=` + string(data) + `, applied={};
function rewrite(raw,method){
 try {
  if(String(method||'GET').toUpperCase()!=='GET')return raw;
  const u=new URL(raw,location.href);
  if(u.origin!=='https://x.com'||!u.pathname.startsWith('/i/api/graphql/'))return raw;
  const op=u.pathname.split('/').pop();
  const phase=['UserTweets','UserOriginalsTimeline'].includes(op)?'posts':['UserTweetsAndReplies','UserRepliesTimeline'].includes(op)?'replies':null;
  if(!phase||applied[phase]||!p[phase].cursor)return raw;
  const v=JSON.parse(u.searchParams.get('variables'));
  if(!v||String(v.userId)!==p.user_id||v.cursor)return raw;
  v.cursor=p[phase].cursor;u.searchParams.set('variables',JSON.stringify(v));applied[phase]=true;return u.href;
 }catch{return raw;}
}
const originalFetch=window.fetch;
window.fetch=function(input,init){
 const isRequest=input instanceof Request, raw=isRequest?input.url:String(input);
 const rewritten=rewrite(raw,init?.method||(isRequest?input.method:'GET'));
 if(rewritten!==raw)input=isRequest?new Request(rewritten,input):rewritten;
 return originalFetch.call(this,input,init);
};
const originalOpen=XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open=function(method,url,...rest){return originalOpen.call(this,method,rewrite(String(url),method),...rest);};
})();`
}
