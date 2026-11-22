#!/system/bin/sh
# Installer hooks (Magisk / KernelSU / APatch).
CONF_DIR=/data/adb/vowifi
MTAG=install
# Module log (same format as the app log: epoch ms, level, message), shown in the WebUI.
MLOG=/data/adb/vowifi/module.log
mlog() {
  mkdir -p /data/adb/vowifi
  [ -f "$MLOG" ] && [ "$(wc -c < "$MLOG")" -gt 524288 ] && mv -f "$MLOG" "$MLOG.1"
  printf '%s000\t%s\t[%s] %s\n' "$(date +%s)" "$1" "$MTAG" "$2" >> "$MLOG"
}
CONF=$CONF_DIR/ims.conf
mkdir -p "$CONF_DIR"
# Written by the app: the slot whose IMS it currently serves (for uninstall).
[ -f "$CONF" ] || echo 'SLOTS=""' > "$CONF"
ui_print "- 打开所选 SIM 卡的系统「WLAN 通话」开关即启用，关闭即恢复原生"
# Make the new APK take effect at the next boot: the package manager caches
# parsed APKs and the module's overlay directories keep an old timestamp, so
# drop the cache; and remove any copy installed over the module in /data
# (adb install), which would otherwise shadow the module's APK.
mlog INFO "安装模块 $(grep '^version=' "$MODPATH/module.prop" | cut -d= -f2)"
rm -rf /data/system/package_cache/* 2>/dev/null && mlog INFO "已清理系统安装缓存"
out=$(pm uninstall-system-updates app.vowifi 2>&1) && mlog INFO "已移除覆盖安装的旧版 App：$out" || mlog DEBUG "没有覆盖安装的旧版 App（$out）"
# ColorOS's app-clone user (and any other secondary user) also gets a copy of
# this persistent system app; it is not needed there.
for u in $(pm list users 2>/dev/null | sed -n 's/.*UserInfo{\([0-9]*\):.*/\1/p'); do
  [ "$u" = 0 ] || { pm uninstall -k --user "$u" app.vowifi >/dev/null 2>&1 && mlog INFO "已从用户 $u（分身等）移除 App"; }
done
set_perm_recursive "$MODPATH/system/priv-app/VoWiFi" 0 0 0755 0644
set_perm "$MODPATH/system/etc/permissions/privapp-permissions-app.vowifi.xml" 0 0 0644
