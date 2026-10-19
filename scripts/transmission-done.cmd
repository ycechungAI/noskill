@echo off
rem Transmission runs this when a download finishes (Windows twin of transmission-done.sh). It passes
rem the torrent's hash in TR_TORRENT_HASH; noskill then files that one torrent now (noskill hook,
rem anime-tracker). Scheduled runs file anything this misses, so a failure here only delays an episode.
rem
rem Put it next to noskill.exe (for example %LOCALAPPDATA%\Programs\noskill) and in Transmission's
rem settings.json set "script-torrent-done-enabled": true and "script-torrent-done-filename" to its full path.
if not defined NOSKILL_HOME set "NOSKILL_HOME=%USERPROFILE%\.noskill"
if not exist "%NOSKILL_HOME%\logs" mkdir "%NOSKILL_HOME%\logs"
echo %date% %time% %TR_TORRENT_NAME%>>"%NOSKILL_HOME%\logs\hook.log"
"%~dp0noskill.exe" hook transmission-finished >>"%NOSKILL_HOME%\logs\hook.log" 2>&1
