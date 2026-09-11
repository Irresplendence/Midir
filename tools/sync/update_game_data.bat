@echo off
setlocal
pushd "%~dp0"

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0update_game_data.ps1" %*

popd
pause
