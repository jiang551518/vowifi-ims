package app.vowifi.data

import java.io.File
import java.util.concurrent.TimeUnit

/**
 * Keeps a root logcat running in the background that writes system logs
 * relevant to WiFi calling (IMS / telephony / Telecom / SMS at full detail,
 * everything else at error level) to files/logs/system.log, rotated
 * (4 × 4 MB). ColorOS floods the ring buffers within minutes, so this is
 * what makes calls and IMS problems diagnosable afterwards.
 */
object SysLogCapture {
    private val tags = listOf(
        "VoWiFi", "ImsResolver", "ImsServiceController", "ImsServiceControllerCompat", "ImsFeatureBinderRepo",
        "ImsPhoneCallTracker", "ImsPhone", "ImsPhoneConnection", "ImsManager", "ImsCall", "ImsCallSession",
        "MmTelFeature", "ImsService", "ImsRegistrationImplBase", "ImsServiceSub", "ImsStateCallbackController",
        "TelephonyConnectionService", "TelephonyConnection", "Telecom", "TelecomFramework", "CallsManager",
        "ImsSmsDispatcher", "SmsDispatchersController", "GsmInboundSmsHandler", "InboundSmsHandler",
        "CarrierConfigLoader", "AndroidRuntime", "DEBUG",
    )

    private fun su(): String =
        listOf("/system/bin/su", "/system/xbin/su", "/sbin/su", "/debug_ramdisk/su").firstOrNull { File(it).exists() } ?: "su"

    /** Starts the capture unless it is already running. Blocking (root call). */
    fun ensureRunning(logDir: File): String {
        val out = File(logDir, "system.log").absolutePath
        val filter = (tags.map { "$it:V" } + "*:E").joinToString(" ")
        // Check real logcat processes only (pgrep -f would match this shell itself).
        val script = "for p in \$(pidof logcat); do grep -q 'system.log' /proc/\$p/cmdline 2>/dev/null && echo running && exit 0; done; " +
            "setsid logcat -b main,system,radio,crash -v threadtime -r 4096 -n 4 -f '$out' $filter " +
            "</dev/null >/dev/null 2>&1 & sleep 1; " +
            "for p in \$(pidof logcat); do grep -q 'system.log' /proc/\$p/cmdline 2>/dev/null && echo started && exit 0; done; echo failed"
        return runCatching {
            val p = ProcessBuilder(su(), "-c", script).redirectErrorStream(true).start()
            val o = p.inputStream.bufferedReader().readText().trim()
            p.waitFor(10, TimeUnit.SECONDS)
            o
        }.getOrElse { "error: ${it.message}" }
    }
}
