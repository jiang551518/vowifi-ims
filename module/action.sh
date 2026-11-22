#!/system/bin/sh
# Module manager "action" button: prints a short status. The full UI is this
# module's WebUI (KernelSU: tap the module; Magisk: KsuWebUI / MMRL).
F=/data/data/app.vowifi/files/webui/status.json
MTAG=action
# Module log (same format as the app log: epoch ms, level, message), shown in the WebUI.
MLOG=/data/adb/vowifi/module.log
mlog() {
  mkdir -p /data/adb/vowifi
  [ -f "$MLOG" ] && [ "$(wc -c < "$MLOG")" -gt 524288 ] && mv -f "$MLOG" "$MLOG.1"
  printf '%s000\t%s\t[%s] %s\n' "$(date +%s)" "$1" "$MTAG" "$2" >> "$MLOG"
}
mlog INFO "模块管理器「操作」按钮"
am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL --es cmd refresh >/dev/null 2>&1
sleep 1
val() { grep -o "\"$1\":\"[^\"]*\"" "$F" 2>/dev/null | head -1 | cut -d'"' -f4; }
if [ ! -f "$F" ]; then
  echo "WiFi 通话服务未运行（请确认模块已安装并重启过手机）"
else
  case "$(val phase)" in
    registered) echo "WiFi 通话：已注册（$(val carrier)）" ;;
    idle|"") echo "WiFi 通话：未开启" ;;
    waiting) echo "WiFi 通话：连接失败，等待重试"; echo "原因：$(val errorText)" ;;
    *) echo "WiFi 通话：正在连接（$(val step)）" ;;
  esac
fi
. /data/adb/vowifi/ims.conf 2>/dev/null
for s in $SLOTS; do
  echo "卡槽 $((s + 1)) 系统 IMS：$(cmd phone ims get-ims-service -s "$s" -c 2>/dev/null)"
done
echo "完整设置：KernelSU 中点击本模块打开 WebUI；Magisk 可用 KsuWebUI 或 MMRL 打开。"
