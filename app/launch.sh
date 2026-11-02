#!/bin/sh
# MiniScale: the Tailscale client's screen (devices, exit node, settings,
# accounts). If the screen's binary is missing, miniscale.sh toggle switches
# Tailscale on and off instead.
cd "$(dirname "$0")"
if [ -x ./miniscale ]; then
    sz=$( { wc -c < miniscale.log; } 2>/dev/null); [ $((${sz:-0})) -gt 65536 ] && mv -f miniscale.log miniscale.log.old
    CPUMIN=/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq
    [ -s /tmp/minideck-cpumin ] || cat $CPUMIN > /tmp/minideck-cpumin 2>/dev/null
    oldmin=$(cat /tmp/minideck-cpumin 2>/dev/null)
    [ -n "$oldmin" ] && [ "$oldmin" -lt 1000000 ] && echo 1000000 > $CPUMIN 2>/dev/null
    restore() { [ -n "$oldmin" ] && echo "$oldmin" > $CPUMIN 2>/dev/null; }
    trap 'restore; exit 0' TERM HUP INT
    ./miniscale 2>> miniscale.log
    restore
else
    sh ./miniscale.sh toggle
fi
