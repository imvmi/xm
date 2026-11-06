'use strict';
// Classify only extreme single photos. Ordinary portraits (including 9:16)
// remain complete; no extra image request or thumbnail generation is needed.
function layoutTimelinePhoto(img){
 if(!img.matches?.('.photo-grid.single > .media-button > img')||!img.naturalWidth||!img.naturalHeight)return;
 const button=img.parentElement,ratio=img.naturalWidth/img.naturalHeight;
 const shape=ratio<.5?'long':ratio>3?'wide':'';
 button.classList.toggle('photo-preview-long',shape==='long');
 button.classList.toggle('photo-preview-wide',shape==='wide');
 let hint=button.querySelector('.photo-preview-hint');
 if(!shape){hint?.remove();return;}
 if(!hint){hint=document.createElement('span');hint.className='photo-preview-hint';button.append(hint);}
 hint.textContent=(shape==='long'?'长图':'宽图')+' · 点击查看完整图片';
}
// Capture load events from lazy images, including newly appended feed pages,
// quotes and reply threads. Also handle images already cached at script startup.
document.addEventListener('load',event=>layoutTimelinePhoto(event.target),true);
document.querySelectorAll('.photo-grid.single > .media-button > img').forEach(layoutTimelinePhoto);
