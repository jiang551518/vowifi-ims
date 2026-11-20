#!/system/bin/sh
# Late start: once Telephony is up, point the configured slots' IMS (MMTEL and
# EMERGENCY_MMTEL, "-f 0,1") at the app. The override is not persistent, so it
# is applied on every boot. Emergency calls are never placed over the app's IMS.
CONF=/data/adb/vowifi/ims.conf
LOG=/data/adb/vowifi/service.log
until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 3; done
sleep 20
SLOTS=""
[ -f "$CONF" ] && . "$CONF"
for s in $SLOTS; do
  for i in 1 2 3 4 5; do
    cmd phone ims set-ims-service -s "$s" -c -f 0,1 app.vowifi >/dev/null 2>&1
    sleep 3
    if [ "$(cmd phone ims get-ims-service -s "$s" -c 2>/dev/null)" = "app.vowifi" ]; then
      echo "$(date '+%F %T') slot $s -> app.vowifi" >> "$LOG"
      break
    fi
    sleep 10
  done
done
