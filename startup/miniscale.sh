#!/bin/sh
# Installed by minideck into /mnt/SDCARD/.tmp_update/startup/.
# Onion runs this at boot; it returns at once and starts Tailscale in the
# background if autostart is on (the app turns it on/off).
APP=/mnt/SDCARD/App/MiniScale
[ -f "$APP/miniscale.sh" ] && setsid nohup sh "$APP/miniscale.sh" boot >/dev/null 2>&1 &
