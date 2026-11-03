#!/bin/sh
# Runs the sched daemon and the local agent monitor.
#
#   ./start.sh               macOS: install the login agents -- or repair any
#                            that are missing, stale (e.g. the project moved)
#                            or stopped -- so they run in the background,
#                            restart if they crash, and start at every login:
#                              com.local.sched           the daemon
#                              local.agent-monitor       status page at
#                                                        http://127.0.0.1:8790
#                              local.agent-monitor.heal  restarts Ollama, the
#                                                        daemon etc. if they're
#                                                        down; at login and
#                                                        every 10 minutes
#                            Then print their status and exit. Elsewhere: same
#                            as --foreground.
#   ./start.sh --restart     same, but always restart them (e.g. to pick up
#                            code changes; config.yaml and task files are
#                            re-read automatically)
#   ./start.sh --status      are the login agents installed and running?
#   ./start.sh --uninstall   stop everything and remove the login agents
#   ./start.sh --foreground  run the daemon in this terminal; Ctrl-C to stop
#   ./start.sh --once        any other arguments go to `sched daemon`, run in
#                            the foreground (--once: run due tasks, then exit)
#   ./start.sh --monitor     what the monitor agents run: `sched monitor serve`
#   ./start.sh --heal        and `sched monitor heal --quiet`
#
# The daemon only runs tasks when they're due -- it serves no web page. For the
# browser UI, run ./web.sh (in another terminal) and open http://127.0.0.1:8787.
# Creates or repairs .venv on the way if needed (see _venv.sh).
set -eu

# cd to the project root -- following symlinks, so this works from any cwd,
# through a symlink (e.g. ~/bin/sched-start), and as `sh/bash/zsh start.sh`.
self=$0
while [ -L "$self" ]; do
    link=$(readlink "$self")
    case $link in
        /*) self=$link ;;
        *) self=$(dirname "$self")/$link ;;
    esac
done
cd "$(dirname "$self")"
ROOT=$(pwd -P)

LABEL=com.local.sched
MONITOR_LABEL=local.agent-monitor
HEAL_LABEL=local.agent-monitor.heal
AGENTS="$LABEL $MONITOR_LABEL $HEAL_LABEL"
AGENTS_DIR="$HOME/Library/LaunchAgents"
PLIST="$AGENTS_DIR/$LABEL.plist"
DOMAIN="gui/$(id -u)"

# Prints an agent's pid (default: the daemon's) if launchd has it running.
agent_pid() {
    launchctl print "$DOMAIN/${1:-$LABEL}" 2>/dev/null | awk '$1 == "pid" && $2 == "=" { print $3; exit }'
}

agent_loaded() {
    launchctl print "$DOMAIN/$1" >/dev/null 2>&1
}

# `sched monitor serve|heal ...`, for the two monitor agents.
run_monitor() {
    . ./_venv.sh
    exec "$VENV_PY" -m schedlm.cli monitor "$@"
}

run_foreground() {
    # launchd sets XPC_SERVICE_NAME to the job's label; anyone else starting a
    # second daemon next to the agent's would race it for the same due tasks.
    if [ "${XPC_SERVICE_NAME:-}" != "$LABEL" ] && command -v launchctl >/dev/null 2>&1; then
        pid=$(agent_pid)
        if [ -n "$pid" ]; then
            echo "Error: the sched daemon is already running in the background (pid $pid)." >&2
            echo "Use './start.sh --status' to check on it, or './start.sh --uninstall' first" >&2
            echo "if you want to run it in this terminal instead." >&2
            exit 1
        fi
    fi
    . ./_venv.sh
    echo "Starting sched daemon (Ctrl-C to stop)..."
    echo "This runs scheduled tasks only; for the web UI, run ./web.sh in another terminal and open http://127.0.0.1:8787"
    # `python -m` rather than .venv/bin/sched, whose shebang hardcodes the venv's
    # absolute path and breaks if the project folder is moved.
    exec "$VENV_PY" -m schedlm.cli daemon "$@"
}

xml_escape() {
    printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# Each agent runs start.sh, so every login also gets the venv check/repair.
#   daemon:  KeepAlive restarts it only if it crashes (non-zero exit)
#   monitor: always kept running
#   heal:    runs at load and every 600 s, then exits
plist_xml() {
    root=$(xml_escape "$ROOT")
    case $1 in
        "$LABEL")
            mode=--foreground out=data/daemon.stdout.log err=data/daemon.stderr.log
            run_keys='    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>30</integer>'
            ;;
        "$MONITOR_LABEL")
            mode=--monitor out=data/monitor/serve.log err=data/monitor/serve.log
            run_keys='    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>30</integer>'
            ;;
        "$HEAL_LABEL")
            mode=--heal out=data/monitor/heal-agent.log err=data/monitor/heal-agent.log
            run_keys='    <key>StartInterval</key>
    <integer>600</integer>'
            ;;
    esac
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$1</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/sh</string>
        <string>$root/start.sh</string>
        <string>$mode</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$root</string>
    <key>RunAtLoad</key>
    <true/>
$run_keys
    <key>StandardOutPath</key>
    <string>$root/$out</string>
    <key>StandardErrorPath</key>
    <string>$root/$err</string>
</dict>
</plist>
EOF
}

show_status() {
    if [ ! -f "$PLIST" ]; then
        echo "Login agents: not installed. Run ./start.sh to install them."
        return 1
    fi
    status=0
    model=$(.venv/bin/python -c 'from schedlm.config import load_config; print(load_config().ollama.model)' 2>/dev/null || echo "?")
    pid=$(agent_pid)
    if [ -n "$pid" ]; then
        echo "sched daemon is running in the background (pid $pid) and starts at every login."
        echo "Default model: $model (change it in the web UI or config.yaml)"
        echo "Logs: data/sched.log   Web UI: ./web.sh -> http://127.0.0.1:8787   Remove: ./start.sh --uninstall"
    else
        echo "Login agent is installed but the daemon is not running. Recent errors (data/daemon.stderr.log):" >&2
        tail -n 8 data/daemon.stderr.log >&2 2>/dev/null || true
        status=1
    fi

    port=$(.venv/bin/python -c 'from schedlm.config import load_config; print(load_config().monitor.port)' 2>/dev/null || echo 8790)
    pid=$(agent_pid "$MONITOR_LABEL")
    if [ -n "$pid" ]; then
        echo "Agent monitor is running (pid $pid) and starts at every login: http://127.0.0.1:$port"
    else
        echo "Agent monitor is not running. Recent output (data/monitor/serve.log):" >&2
        tail -n 8 data/monitor/serve.log >&2 2>/dev/null || true
        status=1
    fi
    if agent_loaded "$HEAL_LABEL"; then
        echo "Self-heal runs at login and every 10 minutes (log: data/monitor/heal.log). Right now:"
    else
        echo "Self-heal agent is not loaded. Right now:" >&2
        status=1
    fi
    checks=$(.venv/bin/python -m schedlm.cli monitor check 2>&1) || status=1
    printf '%s\n' "$checks" | sed 's/^/  /'

    if [ "$status" -ne 0 ]; then
        echo "Run ./start.sh to repair and restart." >&2
    fi
    return "$status"
}

# Writes and (re)loads one agent if its plist is missing or out of date, or
# launchd doesn't have it loaded. Returns 1 if it was already fine.
ensure_agent() {
    plist="$AGENTS_DIR/$1.plist"
    want=$(plist_xml "$1")
    if [ ! -f "$plist" ]; then
        echo "Installing login agent $1 ..."
    elif [ "$(cat "$plist")" != "$want" ]; then
        echo "Repairing login agent $1 ($plist was out of date) ..."
    elif ! agent_loaded "$1"; then
        echo "Login agent $1 wasn't loaded; loading it ..."
    else
        return 1
    fi
    printf '%s\n' "$want" >"$plist"
    # `launchctl disable` outlives reboots and would keep it from loading at login.
    launchctl enable "$DOMAIN/$1" 2>/dev/null || true
    launchctl bootout "$DOMAIN/$1" 2>/dev/null || true
    # bootstrap can fail with "5: Input/output error" while a bootout
    # is still finishing; give it a few tries.
    tries=0
    until launchctl bootstrap "$DOMAIN" "$plist" 2>/dev/null; do
        tries=$((tries + 1))
        if [ "$tries" -ge 5 ]; then
            echo "Error: launchctl could not load $plist" >&2
            launchctl bootstrap "$DOMAIN" "$plist"  # once more, showing its error
            exit 1
        fi
        sleep 1
    done
}

install_agent() {
    restart=$1
    . ./_venv.sh  # build/repair the venv here, where pip's output is visible
    mkdir -p "$AGENTS_DIR" data/monitor
    for label in $AGENTS; do
        if ensure_agent "$label"; then
            continue
        fi
        if [ "$label" = "$HEAL_LABEL" ]; then
            # Not a long-running job; with --restart, run a heal pass now.
            if [ "$restart" = true ]; then
                launchctl kickstart "$DOMAIN/$label"
            fi
        elif [ "$restart" = true ] || [ -z "$(agent_pid "$label")" ]; then
            echo "Restarting $label ..."
            launchctl kickstart -k "$DOMAIN/$label"
        fi
    done

    # Give them a moment to get past the venv check, then report.
    sleep 3
    show_status
}

uninstall_agent() {
    # Heal first, so it can't restart the others while they're removed.
    for label in $HEAL_LABEL $MONITOR_LABEL $LABEL; do
        launchctl bootout "$DOMAIN/$label" 2>/dev/null || true
        rm -f "$AGENTS_DIR/$label.plist"
    done
    echo "Login agents removed; the daemon and monitor are stopped and won't start at login."
}

mode=${1:-}
if [ "$(uname -s)" != Darwin ] || ! command -v launchctl >/dev/null 2>&1; then
    case $mode in
        --restart | --status | --uninstall)
            echo "Error: $mode manages the macOS login agent; on this system use ./start.sh --foreground" >&2
            exit 1
            ;;
        --foreground) shift ;;
        --monitor) run_monitor serve ;;
        --heal) run_monitor heal --quiet ;;
    esac
    run_foreground "$@"
fi

case $mode in
    "") install_agent false ;;
    --restart) install_agent true ;;
    --status) show_status ;;
    --uninstall) uninstall_agent ;;
    --foreground)
        shift
        run_foreground "$@"
        ;;
    --monitor) run_monitor serve ;;
    --heal) run_monitor heal --quiet ;;
    *) run_foreground "$@" ;;
esac
