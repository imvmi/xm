'use strict';
let dataCleanupSelection=null;
function updateDataCleanup(){
 $('data-cleanup-open').disabled=!$('data-cleanup-notifications').checked&&!$('data-cleanup-tasks').checked;
 $('data-cleanup-result').textContent='';
}
for(const id of ['data-cleanup-notifications','data-cleanup-tasks'])$(id).addEventListener('change',updateDataCleanup);
$('data-cleanup-open').addEventListener('click',()=>{
 const notifications=$('data-cleanup-notifications').checked,tasks=$('data-cleanup-tasks').checked;
 if(!notifications&&!tasks)return;
 dataCleanupSelection={notifications,tasks,confirm:true};
 $('data-cleanup-description').textContent='将永久删除'+[notifications?'已结束的通知发送记录':'',tasks?'已结束的任务记录及关联统计':''].filter(Boolean).join('和')+'，包括列表中未显示的历史记录。此操作无法撤销。';
 $('data-cleanup-dialog').showModal();
});
$('data-cleanup-confirm').addEventListener('click',()=>action($('data-cleanup-confirm'),async()=>{
 if(!dataCleanupSelection)return;
 const result=await api('/settings/cleanup','POST',dataCleanupSelection);
 dataCleanupSelection=null;$('data-cleanup-dialog').close();
 $('data-cleanup-result').textContent=`已清理 ${number(result.notifications)} 条通知发送记录、${number(result.runs)} 条任务执行记录和 ${number(result.jobs)} 条已结束任务。`;
 toast('所选历史记录已清理');
}));
