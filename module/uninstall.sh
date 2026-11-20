#!/system/bin/sh
# Runs when the module is removed (at the next boot). Returns the slots to the
# phone's own IMS (the override is not persistent, so a reboot does it too) and
# cleans up what lives outside the module: the app's updated copy in /data
# (from an adb install), its 2G restriction / cross-SIM / voice-state changes,
# and /data/adb/vowifi. Telephony is not up yet here, so that part runs in the
# background once boot has completed.
CONF=/data/adb/vowifi/ims.conf
SLOTS=""
[ -f "$CONF" ] && . "$CONF"
for s in $SLOTS; do
  cmd phone ims clear-ims-service-override -s "$s" >/dev/null 2>&1
done

(
  until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 5; done
  sleep 30
  for s in 0 1; do cmd phone ims clear-ims-service-override -s "$s" >/dev/null 2>&1; done
  if pm path app.vowifi >/dev/null 2>&1; then
    # Let the app undo its own system changes first, then remove it.
    am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL \
      --es cmd restore --es arg uninstall >/dev/null 2>&1
    i=0
    while [ $i -lt 30 ]; do
      grep -q '"restore":"done:uninstall"' /data/data/app.vowifi/files/webui/status.json 2>/dev/null && break
      sleep 3; i=$((i + 1))
    done
    pm uninstall app.vowifi >/dev/null 2>&1
  fi
  rm -rf /data/adb/vowifi
) </dev/null >/dev/null 2>&1 &
