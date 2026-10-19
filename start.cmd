@echo off
rem start.cmd: the noskill menu on Windows (see start.ps1). Runs it with the execution policy relaxed for this one run.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1" %*
exit /b %ERRORLEVEL%
