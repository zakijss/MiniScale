#!/bin/sh
# MiniScale (part of MiniDeck): an unofficial Tailscale client for Miyoo
# Mini / Mini Plus on Onion OS.
#
#   miniscale.sh toggle   on <-> off (off also disables boot); used when
#                        the miniscale screen isn't installed
#   miniscale.sh start    start in the background (logged-in state required;
#                        exits 3 if this device can't open the saved state)
#   miniscale.sh fresh    start for a login (the app then logs in through
#                        tailscaled's local API): with the saved state if it
#                        opens, otherwise empty, the old file kept aside as
#                        state.enc.unreadable
#   miniscale.sh save     save the state now (after a login or a setting)
#   miniscale.sh autostart on|off   start at boot, or not
#   miniscale.sh stop     save state and stop
#   miniscale.sh login    interactive first login (QR code), run inside st
#   miniscale.sh boot     called by Onion at boot: start if autostart is set
#   miniscale.sh status
#   miniscale.sh seal FILE  encrypt stdin (an app's secrets JSON) to FILE with
#                          the device key; the plaintext never touches the card
#
# The node state (private node key, Tailscale SSH host keys) is stored on the
# SD card only encrypted (AES-256-CBC, PBKDF2) with a random key kept in the
# device's internal flash (/appconfigs), so a lost or copied SD card is useless
# on its own. While running, the decrypted state lives in RAM (/tmp) only.

APP=${MINIDECK_APP:-/mnt/SDCARD/App/MiniScale}
RUN=${MINIDECK_RUN:-/tmp/miniscale}
DEVKEY=${MINIDECK_DEVKEY:-/appconfigs/minideck.key}
SYS=/mnt/SDCARD/.tmp_update
OSSL=${MINIDECK_OPENSSL:-$SYS/bin/openssl}
export LD_LIBRARY_PATH="/lib:/config/lib:/mnt/SDCARD/miyoo/lib:$SYS/lib:$SYS/lib/parasyte"
export PATH="$SYS/bin:$PATH"

# PBKDF2 rounds. The device key is 256 random bits, so stretching it adds
# nothing: 4096 rounds take ~0.1 s, where 200000 took ~3 s at every start.
# Files sealed with OLD_ITER still open, and are re-saved with ITER.
ITER=4096
OLD_ITER=200000
SEAL_ITER=$ITER
MAGIC="MIYOONET-STATE-V1"
# start's exit status when the saved state can't be opened on this device
# (the card came from another Miyoo, or the device key is gone). The app
# then runs fresh, which sets the file aside, and asks for a new login.
NOSTATE=3
BIN=$APP/bin/tailscaled
FWD=$APP/bin/miniscale-fwd
BADGEBIN=$APP/bin/miniscale-badge
ENC=$APP/state.enc
AUTO=$APP/autostart
CONF=$APP/miniscale.conf
FWDCONF=$APP/forwards.conf
SOCK=$RUN/tailscaled.sock
STATE=$RUN/tailscaled.state
LOG=$RUN/log
# Held while starting or stopping, so the boot hook and the app can't start
# two tailscaleds at once (the second one unlinks the first one's socket).
LOCK=/tmp/miniscale.lock

# First run (and after updates, which ship only the .example files): create
# the user's own copies.
for f in "$CONF" "$FWDCONF"; do
    [ -f "$f" ] || { [ -f "$f.example" ] && cp "$f.example" "$f"; }
done

# User settings (miniscale.conf), with defaults.
TS_HOSTNAME=miyoo
TS_SSH=true
SOCKS_PORT=1055
HTTP_PROXY_PORT=1056
# The status icon next to Wi-Fi in the Onion menu (position suits themes
# whose Wi-Fi icon sits at about x=507; move it with BADGE_X / BADGE_Y).
BADGE=true
BADGE_X=474
BADGE_Y=18
[ -f "$CONF" ] && . "$CONF"

ts() { "$RUN/tailscale" --socket="$SOCK" "$@"; }
pid_of() { cat "$RUN/$1.pid" 2>/dev/null; }
# A pid file can outlive its process (a crash) and the pid be reused by
# another program, so check the command line too before trusting it (stop
# would otherwise kill that program).
alive() {
    _pid=$(pid_of "$1")
    [ -n "$_pid" ] && kill -0 "$_pid" 2>/dev/null || return 1
    case "$1" in
        tailscaled) _want=tailscaled ;;
        fwd) _want=miniscale-fwd ;;
        badge) _want=miniscale-badge ;;
        *) _want=miniscale.sh ;;
    esac
    case "$(tr '\0' ' ' < "/proc/$_pid/cmdline" 2>/dev/null)" in
        *"$_want"*) return 0 ;;
    esac
    return 1
}
running() { alive tailscaled; }
backend() { ts status --json 2>/dev/null | sed -n 's/.*"BackendState": *"\([A-Za-z]*\)".*/\1/p' | head -1; }
# Logs stay small: miniscale.log on the card starts afresh past 64 KB (the
# previous one kept as .old); the daemons' logs live in RAM (/tmp), so the
# saver loop trims them to their last lines.
cap_log() { _sz=$( { wc -c < "$1"; } 2>/dev/null); [ $((${_sz:-0})) -gt 65536 ] && mv -f "$1" "$1.old"; }
trim_ram_log() {
    _sz=$( { wc -c < "$1"; } 2>/dev/null)
    [ $((${_sz:-0})) -gt 262144 ] || return 0
    tail -n 200 "$1" > "$1.t" && cat "$1.t" > "$1"; rm -f "$1.t"
}
note() { echo "$*"; [ -f "$APP/miniscale.log" ] && cap_log "$APP/miniscale.log"; echo "$(date '+%F %T') $*" >> "$APP/miniscale.log"; }
panel() { infoPanel -t "MiniScale" -m "$1" >/dev/null 2>&1 || true; }

lock() { # waits up to $1 seconds; a lock left by a dead process is taken over
    i=0
    while ! mkdir "$LOCK" 2>/dev/null; do
        holder=$(cat "$LOCK/pid" 2>/dev/null)
        if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then
            rm -rf "$LOCK"; continue
        fi
        [ $i -ge "$1" ] && return 1
        sleep 1; i=$((i+1))
    done
    echo $$ > "$LOCK/pid"
}
unlock() { rm -rf "$LOCK"; }

# Kills tailscaleds started for our socket that no pid file points to (left
# behind by a crash or an older version's double start).
kill_orphans() {
    for p in $(pidof tailscaled 2>/dev/null); do
        [ "$p" = "$(pid_of tailscaled)" ] && running && continue
        tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null | grep -q -- "--socket=$SOCK" && kill -9 "$p" 2>/dev/null
    done
}

ensure_devkey() {
    [ -s "$DEVKEY" ] && return 0
    umask 077
    "$OSSL" rand -hex 32 > "$DEVKEY.tmp" && mv -f "$DEVKEY.tmp" "$DEVKEY" && sync
}

# RAM -> encrypted file on the card. Callers hold the lock: every save goes
# through the same temporary files. The archive is written to RAM and its
# status checked before encrypting, because in a pipe a failed tar would go
# unnoticed (ash has no pipefail) and a cut-short state would replace the
# good one. The new file replaces the old only once it decrypts back to the
# same bytes.
save_state() {
    [ -f "$STATE" ] && [ -s "$DEVKEY" ] || return 1
    _files="tailscaled.state"; [ -d "$RUN/ssh" ] && _files="$_files ssh"
    _plain="$RUN/save.tar" _check="$RUN/save.chk"
    { echo "$MAGIC" && tar -C "$RUN" -cf - $_files; } > "$_plain" \
        && "$OSSL" enc -aes-256-cbc -pbkdf2 -iter $ITER -salt -pass "file:$DEVKEY" \
            -in "$_plain" -out "$ENC.tmp" 2>/dev/null \
        && "$OSSL" enc -d -aes-256-cbc -pbkdf2 -iter $ITER -pass "file:$DEVKEY" \
            -in "$ENC.tmp" -out "$_check" 2>/dev/null \
        && [ "$("$OSSL" dgst -sha256 < "$_check")" = "$("$OSSL" dgst -sha256 < "$_plain")" ] \
        && mv -f "$ENC.tmp" "$ENC" && sync && touch "$RUN/saved"
    _rc=$?
    rm -f "$_plain" "$_check" "$ENC.tmp"
    return $_rc
}

load_state() { # encrypted file on the card -> RAM
    [ -f "$ENC" ] && [ -s "$DEVKEY" ] || return 1
    dec="$RUN/dec.$$"
    for it in $ITER $OLD_ITER; do
        "$OSSL" enc -d -aes-256-cbc -pbkdf2 -iter $it -pass "file:$DEVKEY" -in "$ENC" -out "$dec" 2>/dev/null \
            && [ "$(head -n 1 "$dec")" = "$MAGIC" ] && break
        rm -f "$dec"
    done
    [ -f "$dec" ] && tail -n +2 "$dec" | tar -C "$RUN" -xf - && [ -f "$STATE" ]
    rc=$?
    rm -f "$dec"
    # an old-format file: re-save it in the quick format
    [ $rc -eq 0 ] && [ "$it" = "$OLD_ITER" ] && save_state
    return $rc
}

# Loads the saved state, if there is one. A file this device can't open is
# moved aside (state.enc.unreadable) rather than left to be overwritten by
# the next login, so it can still go back to the Miyoo it came from. Fails
# only when openssl itself doesn't run, as then nothing can be judged.
load_or_set_aside() {
    [ -f "$ENC" ] || return 0
    load_state && return 0
    "$OSSL" version >/dev/null 2>&1 || { note "can't run $OSSL"; return 1; }
    # a partly unpacked state must not be mistaken for a login
    rm -rf "$STATE" "$RUN/ssh"
    aside="$ENC.unreadable" n=1
    while [ -e "$aside" ]; do n=$((n+1)); aside="$ENC.unreadable.$n"; done
    mv -f "$ENC" "$aside" && sync
    note "saved state can't be opened on this device (kept as ${aside##*/}): log in again"
}

# Taildrop: received files are staged in the state directory, which is in
# RAM here; point that at the card instead (the app saves them to TAILDROP).
TAILDROP=/mnt/SDCARD/Media/Taildrop

prepare() {
    mkdir -p "$RUN" && chmod 700 "$RUN"
    ln -sf "$BIN" "$RUN/tailscale"
    rm -f "$SOCK"
    mkdir -p "$TAILDROP/.inbox" 2>/dev/null && ln -sfn "$TAILDROP/.inbox" "$RUN/files"
}

start_daemons() {
    # Logs are opened for appending, so trim_ram_log can shorten them while
    # the daemons write (a plain > would keep writing at the old offset).
    : > "$LOG"; : > "$RUN/fwd.log"; : > "$RUN/badge.log"
    setsid nohup "$BIN" --tun=userspace-networking --statedir="$RUN" --state="$STATE" \
        --socket="$SOCK" --socks5-server="127.0.0.1:$SOCKS_PORT" \
        --outbound-http-proxy-listen="127.0.0.1:$HTTP_PROXY_PORT" >> "$LOG" 2>&1 &
    echo $! > "$RUN/tailscaled.pid"
    i=0; while [ ! -S "$SOCK" ] && [ $i -lt 100 ]; do usleep 200000; i=$((i+1)); done
    if [ -f "$FWDCONF" ] && grep -q '^[[:space:]]*[0-9]' "$FWDCONF"; then
        setsid nohup "$FWD" -c "$FWDCONF" -socks "127.0.0.1:$SOCKS_PORT" >> "$RUN/fwd.log" 2>&1 &
        echo $! > "$RUN/fwd.pid"
    fi
    if [ "$BADGE" = "true" ] && [ -x "$BADGEBIN" ]; then
        setsid nohup nice -n 10 "$BADGEBIN" -socket "$SOCK" -x "$BADGE_X" -y "$BADGE_Y" >> "$RUN/badge.log" 2>&1 &
        echo $! > "$RUN/badge.pid"
    fi
    # Re-save the state when tailscaled changes it (e.g. key renewal).
    setsid nohup sh "$APP/miniscale.sh" saver > /dev/null 2>&1 &
    echo $! > "$RUN/saver.pid"
}

wait_backend() { # $1 = seconds; prints final state
    i=0; b=""
    while [ $i -lt $(($1 * 4)) ]; do
        b=$(backend)
        case "$b" in Running|NeedsLogin|Stopped|NeedsMachineAuth) break ;; esac
        usleep 250000; i=$((i+1))
    done
    echo "$b"
}

cmd_start() {
    lock 120 || { note "another start is still running"; return 1; }
    do_start; rc=$?
    unlock
    return $rc
}

do_start() {
    running && { note "already running"; return 0; }
    kill_orphans
    [ -f "$ENC" ] || { note "not logged in yet: open the app to log in"; return 1; }
    [ -s "$DEVKEY" ] || { note "device key missing ($DEVKEY): log in again"; return $NOSTATE; }
    prepare
    load_state || { note "could not decrypt state (different device or flash wiped): log in again"; rm -rf "$RUN"; return $NOSTATE; }
    start_daemons
    b=$(wait_backend 30)
    note "started: backend=$b ip=$(ts ip -4 2>/dev/null)"
    # (links are left out: a login link must not end up on the card)
    running || note "tailscaled exited: $(tail -n 3 "$LOG" 2>/dev/null | sed 's#https*://[^ ]*#(link)#g' | tr '\n' ' ')"
    [ "$b" = "Running" ]
}

cmd_stop() {
    lock 60 || { note "busy: try again"; return 1; }
    do_stop; rc=$?
    unlock
    return $rc
}

# Fails when the state couldn't be saved, though everything is stopped.
do_stop() {
    _saved=0
    if running; then
        save_state || { note "warning: state not saved"; _saved=1; }
    fi
    for d in saver badge fwd tailscaled; do alive $d && kill "$(pid_of $d)" 2>/dev/null; done
    i=0; while running && [ $i -lt 50 ]; do usleep 200000; i=$((i+1)); done
    running && kill -9 "$(pid_of tailscaled)" 2>/dev/null
    kill_orphans
    rm -rf "$RUN"
    note "stopped"
    [ $_saved -eq 0 ] || echo "stopped, but the state couldn't be saved"
    return $_saved
}

cmd_login() { # interactive, inside st
    clear 2>/dev/null
    echo "== MiniScale: join your tailnet =="
    ensure_devkey || { echo "Could not create the device key."; return 1; }
    running && cmd_stop >/dev/null
    lock 60 || { echo "MiniScale is busy starting: try again in a minute."; return 1; }
    prepare
    load_or_set_aside || { unlock; echo "Could not open the saved state."; return 1; }
    start_daemons
    wait_backend 30 >/dev/null
    if [ "$(backend)" != "Running" ]; then
        echo "Scan the QR code with your phone, or open the link:"
        ssh_flag=""; [ "$TS_SSH" = "true" ] && ssh_flag="--ssh"
        ts up $ssh_flag --hostname="$TS_HOSTNAME" --qr --qr-format=small --timeout=300s || {
            echo "Login failed or timed out."; unlock; cmd_stop >/dev/null; return 1; }
    fi
    save_state; rc=$?
    unlock
    [ $rc -eq 0 ] || { echo "Could not save state."; return 1; }
    touch "$AUTO"; sync
    echo
    echo "MiniScale is ON: $(ts ip -4 2>/dev/null) ($TS_HOSTNAME)"
    echo "It will start automatically at boot."
}

cmd_fresh() {
    lock 60 || { note "busy: try again"; return 1; }
    rc=0
    if running; then
        note "already running"
    else
        kill_orphans
        if ensure_devkey; then
            prepare
            # a saved login (even a logged-out one) is used if it opens
            if load_or_set_aside; then
                start_daemons
                note "started for login: backend=$(wait_backend 30)"
            else
                rm -rf "$RUN"; rc=1
            fi
        else
            note "could not create the device key"; rc=1
        fi
    fi
    unlock
    return $rc
}

cmd_save() {
    running || return 1
    lock 30 || return 1
    save_state; rc=$?
    unlock
    [ $rc -eq 0 ] && note "state saved" || note "warning: state not saved"
    return $rc
}

cmd_autostart() {
    case "$1" in
        on) touch "$AUTO" ;;
        off) rm -f "$AUTO" ;;
        *) [ -f "$AUTO" ] && echo on || echo off; return 0 ;;
    esac
    sync
}

cmd_toggle() {
    if running; then
        rm -f "$AUTO"; sync
        cmd_stop
        panel "OFF (won't start at boot)"
    elif [ ! -f "$ENC" ] || [ ! -s "$DEVKEY" ]; then
        st -q -e sh -c "sh '$APP/miniscale.sh' login; printf '\nPress START to close.'; read _"
    elif [ -d "$LOCK" ]; then
        # The boot hook is starting it right now: wait for that instead.
        infoPanel -t "MiniScale" -m "Starting..." --persistent >/dev/null 2>&1 &
        panel_pid=$!
        lock 120 && unlock
        kill -9 "$panel_pid" 2>/dev/null
        if running; then panel "ON: $(ts ip -4 2>/dev/null)"; else panel "Failed to start: see miniscale.log"; fi
    else
        touch "$AUTO"; sync
        infoPanel -t "MiniScale" -m "Starting..." --persistent >/dev/null 2>&1 &
        panel_pid=$!
        cmd_start; rc=$?
        kill -9 "$panel_pid" 2>/dev/null
        if [ $rc -eq 0 ]; then
            panel "ON: $(ts ip -4 2>/dev/null)"
        elif [ $rc -eq $NOSTATE ]; then
            panel "This Miyoo can't open the saved login: log in again"
            st -q -e sh -c "sh '$APP/miniscale.sh' login; printf '\nPress START to close.'; read _"
        else
            panel "Failed to start: see miniscale.log"
        fi
    fi
}

cmd_boot() {
    [ -f "$AUTO" ] || exit 0
    # Wait up to 60 s for Wi-Fi to get an address.
    i=0; while [ $i -lt 120 ] && ! ip route 2>/dev/null | grep -q '^default'; do usleep 500000; i=$((i+1)); done
    cmd_start
}

cmd_saver() {
    while running; do
        sleep 300
        # A start, a stop or the app's save may hold the lock: then this
        # round is skipped, and the next one saves.
        if [ -f "$STATE" ] && [ "$STATE" -nt "$RUN/saved" ] && lock 5; then
            save_state; unlock
        fi
        for l in "$LOG" "$RUN/fwd.log" "$RUN/badge.log"; do trim_ram_log "$l"; done
    done
}

cmd_seal() {
    out=${1:?usage: miniscale.sh seal OUTFILE < secrets.json}
    ensure_devkey || { echo "no device key" >&2; return 1; }
    "$OSSL" enc -aes-256-cbc -pbkdf2 -iter $SEAL_ITER -salt -pass "file:$DEVKEY" -out "$out.tmp" \
        && mv -f "$out.tmp" "$out" && sync && echo "sealed: $out"
}

cmd_status() {
    if running; then
        echo "running: $(backend) $(ts ip -4 2>/dev/null)"
        alive fwd && echo "forwarder: on ($(grep -c '^[[:space:]]*[0-9]' "$FWDCONF") ports)"
    else
        echo "stopped"
    fi
    [ -f "$AUTO" ] && echo "autostart: on" || echo "autostart: off"
}

case "$1" in
    toggle) cmd_toggle ;;
    start)  cmd_start ;;
    fresh)  cmd_fresh ;;
    save)   cmd_save ;;
    autostart) cmd_autostart "$2" ;;
    stop)   cmd_stop ;;
    login)  cmd_login ;;
    boot)   cmd_boot ;;
    saver)  cmd_saver ;;
    status) cmd_status ;;
    seal)   cmd_seal "$2" ;;
    *) echo "usage: $0 toggle|start|fresh|save|autostart [on|off]|stop|login|boot|status|seal FILE"; exit 2 ;;
esac
