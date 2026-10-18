#!/bin/sh
# Transmission runs this when a download finishes. It passes the torrent's hash in TR_TORRENT_HASH;
# noskill then files that one torrent now (noskill hook, anime-tracker). Scheduled runs file anything
# this misses, so a failure here only delays an episode.
#
# Put it next to the noskill binary (~/.noskill/bin) and make it executable, then in Transmission:
#   macOS: Settings › Transfers › Management › "Call script when download completes"
#   Linux / Windows: set "script-torrent-done-enabled": true and "script-torrent-done-filename" in settings.json
bin="$(cd "$(dirname "$0")" && pwd)/noskill"
log="$HOME/.noskill/logs/hook.log"
{
  printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "${TR_TORRENT_NAME:-?}"
  "$bin" hook transmission-finished 2>&1
} >>"$log" 2>&1
