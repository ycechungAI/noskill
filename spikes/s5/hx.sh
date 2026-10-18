#!/bin/sh
# S5: run a Hermes command inside the sandbox, with a throwaway HOME and a clean environment.
#   spikes/s5/hx.sh hermes --version
S="$(cd "$(dirname "$0")" && pwd)"; H="$S/out/home"
exec env -i HOME="$H" PATH="$H/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin" TERM=dumb LANG=en_US.UTF-8 NO_COLOR=1 $HX_ENV \
  /usr/bin/sandbox-exec -f "$S/sandbox-run.sb" "$@"
