@echo off
setlocal
cd /d "%~dp0"
if not exist "data\xm.db" (
  echo Missing data\xm.db. Finish downloading the migration package first.
  pause
  exit /b 1
)
if not exist "data\media\" (
  echo Missing data\media. The core ZIP alone is not a complete migration.
  pause
  exit /b 1
)
echo XM - Windows x64
echo Open http://127.0.0.1:9980 in Chrome after the server starts.
echo Keep this window open. Press Ctrl+C to stop XM.
"%~dp0xm.exe" serve -config "%~dp0config.yaml" -listen 127.0.0.1:9980
set "xmExit=%errorlevel%"
if not "%xmExit%"=="0" pause
exit /b %xmExit%
