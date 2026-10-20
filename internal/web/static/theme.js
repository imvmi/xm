'use strict';
// Run before styles/body paint; the server supplies the saved preference.
let archiveThemePreference=document.documentElement.dataset.themePreference||'system';
const systemTheme=window.matchMedia?.('(prefers-color-scheme: dark)');
function applyArchiveTheme(preference=archiveThemePreference){
  archiveThemePreference=['system','light','dark'].includes(preference)?preference:'system';
  const theme=archiveThemePreference==='dark'||(archiveThemePreference==='system'&&systemTheme?.matches)?'dark':'light';
  document.documentElement.dataset.theme=theme;
  document.documentElement.dataset.themePreference=archiveThemePreference;
  document.querySelector('meta[name="color-scheme"]')?.setAttribute('content',theme);
}
applyArchiveTheme();
systemTheme?.addEventListener('change',()=>{if(archiveThemePreference==='system')applyArchiveTheme();});
