package app.vowifi.control

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Base64
import app.vowifi.Core
import org.json.JSONObject

/**
 * Commands from the module's WebUI, sent through root:
 *   am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL --es cmd CMD [--es arg ARG]
 * The manifest requires senders to hold MODIFY_PHONE_STATE (root/system), so
 * ordinary apps cannot drive it. Results show up in files/webui/status.json.
 */
class ControlReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val cmd = intent.getStringExtra("cmd") ?: return
        val arg = intent.getStringExtra("arg").orEmpty()
        if (cmd != "refresh" && cmd != "cell" && cmd != "exitip") {
            // Every WebUI action is logged (the settings payload is summarized).
            Core.appendLog("INFO", "WebUI 命令：$cmd${if (arg.isEmpty()) "" else if (cmd == "save") "（设置）" else " $arg"}")
        }
        val result = runCatching {
            when (cmd) {
                "refresh" -> { Core.refreshSims(); WebBridge.touch() }
                "start" -> WebBridge.startWifiCalling().takeIf { it.isNotEmpty() }?.let { note(it) }
                "stop" -> WebBridge.stopWifiCalling()
                "restart" -> Core.restartWifiCalling(Core.app)
                "sim" -> arg.toIntOrNull()?.let { WebBridge.selectSim(it) }
                "net" -> WebBridge.setNetworkMode(arg)
                "exitip" -> WebBridge.checkExitIp()
                "radio" -> WebBridge.setRadioOff(arg == "1" || arg == "true")
                "cell" -> WebBridge.refreshCell()
                "volte" -> app.vowifi.WfcSync.setVolteSync(arg == "1" || arg == "true")
                "autostart" -> WebBridge.setAutoStart(arg == "1" || arg == "true")
                "debug" -> WebBridge.setDebugLog(arg == "1" || arg == "true")
                "save" -> WebBridge.saveSettings(JSONObject(String(Base64.decode(arg, Base64.DEFAULT))))
                "clearlogs" -> Core.clearLogs()
                "restore" -> WebBridge.restoreNative(arg)
                else -> Core.appendLog("WARN", "WebUI：未知命令 $cmd")
            }
        }
        result.exceptionOrNull()?.let { Core.appendLog("ERROR", "WebUI 命令 $cmd 失败：${it.message}") }
    }

    private fun note(msg: String) {
        WebBridge.extra.value = WebBridge.extra.value.copy(note = msg)
    }
}
