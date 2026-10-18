#!/bin/sh
# S5: run Hermes's own installer stage by stage in the sandbox, timing each stage.
#   spikes/s5/install_hermes.sh [stage ...]      (default: all non-interactive stages)
S="$(cd "$(dirname "$0")" && pwd)"
OUT="$S/out"; H="$OUT/home"
mkdir -p "$H"
for stage in ${@:-prerequisites repository venv python-deps config products complete}; do
  t0=$(date +%s)
  env -i HOME="$H" PATH=/usr/bin:/bin:/usr/sbin:/sbin TERM=dumb LANG=en_US.UTF-8 \
    /usr/bin/sandbox-exec -f "$S/sandbox-install.sb" /bin/bash "$S/hermes-agent/scripts/install.sh" \
    --non-interactive --hermes-home "$H/.hermes" --dir "$OUT/hermes-install" \
    --skip-browser --skip-computer-use --stage "$stage" > "$OUT/install-$stage.log" 2>&1
  rc=$?
  t1=$(date +%s)
  printf '{"stage":"%s","rc":%s,"seconds":%s}\n' "$stage" "$rc" "$((t1 - t0))" >> "$OUT/install-times.jsonl"
  echo "stage $stage rc=$rc $((t1 - t0))s"
  [ "$rc" = 0 ] || { tail -5 "$OUT/install-$stage.log"; exit "$rc"; }
done
