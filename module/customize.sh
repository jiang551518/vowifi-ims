#!/system/bin/sh
# Installer hooks (Magisk / KernelSU / APatch).
CONF_DIR=/data/adb/vowifi
CONF=$CONF_DIR/ims.conf
mkdir -p "$CONF_DIR"
# Written by the app: the slot whose IMS it currently serves (for uninstall).
[ -f "$CONF" ] || echo 'SLOTS=""' > "$CONF"
ui_print "- 打开所选 SIM 卡的系统「WLAN 通话」开关即启用，关闭即恢复原生"
# Make the new APK take effect at the next boot: the package manager caches
# parsed APKs and the module's overlay directories keep an old timestamp, so
# drop the cache; and remove any copy installed over the module in /data
# (adb install), which would otherwise shadow the module's APK.
rm -rf /data/system/package_cache/* 2>/dev/null
pm uninstall-system-updates app.vowifi >/dev/null 2>&1
# ColorOS's app-clone user (and any other secondary user) also gets a copy of
# this persistent system app; it is not needed there.
for u in $(pm list users 2>/dev/null | sed -n 's/.*UserInfo{\([0-9]*\):.*/\1/p'); do
  [ "$u" = 0 ] || pm uninstall -k --user "$u" app.vowifi >/dev/null 2>&1
done
set_perm_recursive "$MODPATH/system/priv-app/VoWiFi" 0 0 0755 0644
set_perm "$MODPATH/system/etc/permissions/privapp-permissions-app.vowifi.xml" 0 0 0644
