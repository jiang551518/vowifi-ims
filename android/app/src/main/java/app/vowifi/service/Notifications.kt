package app.vowifi.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import app.vowifi.Core
import app.vowifi.EngineStatus
import app.vowifi.R

object Notifications {
    const val SERVICE_ID = 1
    private const val CH_SERVICE = "service"
    private const val CH_SMS = "sms"
    private const val CH_CALL_HINT = "call_hint"

    fun ensureChannels(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CH_SERVICE, "WiFi 通话 状态", NotificationManager.IMPORTANCE_LOW))
        nm.createNotificationChannel(NotificationChannel(CH_SMS, "短信", NotificationManager.IMPORTANCE_HIGH))
        nm.createNotificationChannel(NotificationChannel(CH_CALL_HINT, "通话失败提示", NotificationManager.IMPORTANCE_HIGH))
        // Calls now ring and show in the system phone app; drop the old app channels.
        nm.deleteNotificationChannel("call")
        nm.deleteNotificationChannel("missed_calls")
    }

    private fun label(st: EngineStatus): Pair<String, String> = when (st.phase) {
        "registered" -> "WiFi 通话 已就绪" to listOf(st.number, st.carrier).filter { it.isNotEmpty() }.joinToString(" · ")
        "registering" -> "正在注册 IMS…" to st.carrier
        "tunnel" -> "隧道已建立" to st.epdg
        "connecting" -> "正在连接…" to st.message
        "waiting" -> "连接中断，等待重试" to st.error
        else -> "WiFi 通话 未启用" to ""
    }

    fun service(ctx: Context, st: EngineStatus, closing: Boolean = false): Notification {
        val (title, text) = if (closing) "正在关闭 WiFi 通话…" to "交还系统 IMS 并恢复原生通话" else label(st)
        val stop = PendingIntent.getService(ctx, 1,
            Intent(ctx, VoWiFiService::class.java).setAction(VoWiFiService.ACTION_SWITCH_OFF), PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(ctx, CH_SERVICE)
            .setSmallIcon(R.drawable.ic_launcher_fg)
            .setContentTitle(title)
            .setContentText(text)
            .setOngoing(true)
            .addAction(Notification.Action.Builder(null, "关闭 WiFi 通话", stop).build())
            .build()
    }

    fun updateService(ctx: Context, st: EngineStatus, closing: Boolean = false) {
        ctx.getSystemService(NotificationManager::class.java).notify(SERVICE_ID, service(ctx, st, closing))
    }

    /** Why a call failed, when the system dialer only says "call ended". */
    fun showCallHint(ctx: Context, title: String, text: String) {
        ensureChannels(ctx)
        val n = Notification.Builder(ctx, CH_CALL_HINT)
            .setSmallIcon(R.drawable.ic_launcher_fg)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(CALL_HINT_ID, n)
    }

    private const val CALL_HINT_ID = 900

    fun showSms(ctx: Context, from: String, text: String) {
        val n = Notification.Builder(ctx, CH_SMS)
            .setSmallIcon(R.drawable.ic_launcher_fg)
            .setContentTitle(from)
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(1000 + (from.hashCode() and 0xffff), n)
    }

}

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        // The app process is persistent (started by the system at boot); this
        // receiver only makes sure it is up after boot or an app update. Whether
        // WiFi calling runs is decided by the SIM's system WLAN calling switch.
        app.vowifi.WfcSync.request("boot", 2000, refresh = true)
    }
}
