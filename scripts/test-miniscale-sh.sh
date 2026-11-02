#!/bin/sh
# Tests miniscale.sh's state handling (save, load, set-aside, locks) under
# BusyBox ash, the shell Onion runs it with. Needs openssl. Run through
#   make shtest
# which runs it in an Alpine container with the repo mounted at /src.
T=$(mktemp -d)
export MINIDECK_APP=$T/app MINIDECK_RUN=$T/run MINIDECK_DEVKEY=$T/devkey MINIDECK_OPENSSL=openssl
mkdir -p "$T/app" "$T/run"
fail=0
check() { if eval "$2"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi; }
set -- status
. "${SRC:-/src}/app/miniscale.sh" >/dev/null
OSSL=openssl

ensure_devkey
echo "node state v1" > "$STATE"; mkdir -p "$RUN/ssh"; echo hostkey > "$RUN/ssh/key"

# 1. a normal save round-trips
save_state; check "save succeeds" '[ $? -eq 0 ] && [ -f "$ENC" ]'
check "no temporary files left" '[ ! -e "$ENC.tmp" ] && [ ! -e "$RUN/save.tar" ] && [ ! -e "$RUN/save.chk" ]'
good=$(md5sum < "$ENC")
rm -rf "$RUN/tailscaled.state" "$RUN/ssh"
load_state; check "load restores the files" '[ "$(cat "$STATE")" = "node state v1" ] && [ "$(cat "$RUN/ssh/key")" = hostkey ]'

# 2. a failing tar leaves the good file alone
echo "node state v2" > "$STATE"
tar() { echo partial; return 1; }
save_state; rc=$?
unset -f tar
check "tar failure fails the save" '[ $rc -ne 0 ]'
check "tar failure keeps state.enc" '[ "$(md5sum < "$ENC")" = "$good" ] && [ ! -e "$ENC.tmp" ]'

# 3. an encryption whose output is cut short is caught by the check
mkdir -p "$T/bin"
cat > "$T/bin/badssl" <<'EOF'
#!/bin/sh
openssl "$@" || exit
case " $* " in *" -d "*) ;; *" enc "*)
    for a; do [ "$prev" = -out ] && { head -c 40 "$a" > "$a.x"; mv "$a.x" "$a"; }; prev=$a; done ;;
esac
EOF
chmod +x "$T/bin/badssl"
OSSL=$T/bin/badssl
save_state; rc=$?
OSSL=openssl
check "a cut-short file fails the save" '[ $rc -ne 0 ]'
check "and keeps state.enc" '[ "$(md5sum < "$ENC")" = "$good" ] && [ ! -e "$ENC.tmp" ]'

# 4. save_state doesn't clobber its caller's rc (load_state re-saves)
rc=0; save_state; check "save works again" '[ $? -eq 0 ]'; check "caller rc kept" '[ $rc -eq 0 ]'

# 5. an unreadable state is set aside, never loaded
good=$(md5sum < "$ENC")
rm -rf "$RUN/tailscaled.state" "$RUN/ssh"
mv "$DEVKEY" "$DEVKEY.old"; ensure_devkey   # as if the flash was wiped
load_or_set_aside; rc=$?
check "set-aside succeeds" '[ $rc -eq 0 ]'
check "state.enc moved to state.enc.unreadable" '[ ! -e "$ENC" ] && [ "$(md5sum < "$ENC.unreadable")" = "$good" ]'
check "nothing loaded" '[ ! -e "$STATE" ] && [ ! -e "$RUN/ssh" ]'
echo junk > "$ENC"
load_or_set_aside
check "a second one doesn't overwrite the first" '[ "$(md5sum < "$ENC.unreadable")" = "$good" ] && [ "$(cat "$ENC.unreadable.2")" = junk ]'
check "no state.enc: nothing to do" 'load_or_set_aside'

# 6. a readable one loads
mv "$ENC.unreadable" "$ENC"; mv -f "$DEVKEY.old" "$DEVKEY"
load_or_set_aside
check "readable state loads" '[ -f "$ENC" ] && [ "$(cat "$STATE")" = "node state v2" ]'

# 7. openssl missing: nothing is moved
rm -rf "$RUN/tailscaled.state"
OSSL=/nonexistent/openssl
load_or_set_aside >/dev/null; rc=$?
OSSL=openssl
check "no openssl: fails, keeps state.enc" '[ $rc -ne 0 ] && [ -f "$ENC" ]'

# 8. start exits NOSTATE when the state can't be opened
mv "$DEVKEY" "$DEVKEY.old"
do_start >/dev/null; rc=$?
check "start without the device key exits $NOSTATE" '[ $rc -eq $NOSTATE ]'
ensure_devkey
do_start >/dev/null; rc=$?
check "start with another key exits $NOSTATE" '[ $rc -eq $NOSTATE ] && [ -f "$ENC" ]'

# 9. the saver skips a round while another save holds the lock
LOCK=$T/lock
sleep 30 & holder=$!
mkdir "$LOCK"; echo $holder > "$LOCK/pid"
t0=$(date +%s); lock 2; rc=$?; t1=$(date +%s)
check "lock while held fails after the wait" '[ $rc -ne 0 ] && [ $((t1 - t0)) -le 4 ]'
kill $holder; wait $holder 2>/dev/null
lock 0; check "a dead holder's lock is taken over" '[ $? -eq 0 ]'; unlock

rm -rf "$T"
exit $fail
