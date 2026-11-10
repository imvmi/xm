'use strict';
state.followingPending=new Set();
$('profile-header').addEventListener('click',async event=>{
 const button=event.target.closest('#profile-follow');if(!button||button.disabled)return;
 const target=state.target,u=state.users.find(u=>u.username===target);
 if(!u||state.followingPending.has(target))return;
 const csrf=state.csrf;
 state.followingPending.add(target);button.disabled=true;
 try{
  const result=await api('/users/'+encodeURIComponent(target)+'/follow','PUT',{followed:!u.followed});
  if(!state.authenticated||state.csrf!==csrf)return;
  // Discard directory reads started before this write, including cached profiles.
  userRequest++;userDirectoryCache=null;
  state.users=state.users.map(account=>account.username===target?{...account,followed:result.followed}:account);
  toast(result.followed?'已关注，可在主时间线「关注」中查看':'已取消关注');
  // Restart reads invalidated above, even if navigation changed during the save.
  if(state.view==='user'||state.view==='archive')void refreshUsers(state.view==='user'?state.target:'').catch(err=>toast(err.message,true));
  if(state.view==='home'&&state.timelineTab==='following')await refreshPosts();
 }catch(err){if(state.authenticated&&state.csrf===csrf)toast(err.message,true);}
 finally{
  state.followingPending.delete(target);
  if(state.authenticated&&state.csrf===csrf&&state.view==='user'&&state.target===target){
   const restore=document.activeElement===button||document.activeElement===document.body;
   renderProfile();if(restore)$('profile-follow')?.focus({preventScroll:true});
  }
 }
});
