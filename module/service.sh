#!/system/bin/sh
# Late start. The app (a persistent system app) decides on its own which SIM
# slot's IMS it serves: the chosen SIM's system "WLAN calling" switch on = take
# the slot over and connect, off = hand it back to the phone's own IMS. Here we
# only make sure the app process is up once boot has completed. The takeover is
# never persistent, so without the app every slot stays with the phone's IMS.
MTAG=boot
# Module log (same format as the app log: epoch ms, level, message), shown in the WebUI.
MLOG=/data/adb/vowifi/module.log
mlog() {
  mkdir -p /data/adb/vowifi
  [ -f "$MLOG" ] && [ "$(wc -c < "$MLOG")" -gt 524288 ] && mv -f "$MLOG" "$MLOG.1"
  printf '%s000\t%s\t[%s] %s\n' "$(date +%s)" "$1" "$MTAG" "$2" >> "$MLOG"
}
until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 3; done
mlog INFO "开机完成"
sleep 20
# ColorOS's app-clone user (and any other secondary user) also gets a copy of
# this persistent system app; it is not needed there.
for u in $(pm list users 2>/dev/null | sed -n 's/.*UserInfo{\([0-9]*\):.*/\1/p'); do
  [ "$u" = 0 ] || { pm uninstall -k --user "$u" app.vowifi >/dev/null 2>&1 && mlog INFO "已从用户 $u（分身等）移除 App"; }
done
am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL --es cmd refresh >/dev/null 2>&1
if pidof app.vowifi >/dev/null; then mlog INFO "App 已在运行（进程 $(pidof app.vowifi)）"
else mlog ERROR "App 未运行：请检查 root 授权，或查看 /data/data/app.vowifi/files/logs/app.log"; fi
