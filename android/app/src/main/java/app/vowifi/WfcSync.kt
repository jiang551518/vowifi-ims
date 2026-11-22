package app.vowifi

import android.database.ContentObserver
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.telephony.SubscriptionManager
import app.vowifi.service.VoWiFiService
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit

/**
 * The system "WLAN calling" switch of the chosen SIM (Settings > SIM) is the
 * module's master switch:
 *  - on:  this app's IMS service takes the SIM's slot over and connects;
 *  - off: WiFi calling stops, the app undoes its system changes and the slot
 *         goes back to the phone's own IMS (native VoLTE etc.).
 * The WebUI switch only flips the system switch, so both always agree (and the
 * ColorOS status bar, which needs the system switch on, shows the icon).
 *
 * Runs in the persistent app process: it watches the switch (subscription
 * database) even while the slot belongs to the phone's own IMS, when Telephony
 * has no reason to tell this app anything.
 */
object WfcSync {
    private val exec = Executors.newSingleThreadScheduledExecutor()
    private var pending: ScheduledFuture<*>? = null
    @Volatile private var ready = false

    /** Slot whose IMS this app took over, as last applied (-1 = none). */
    private var ownedSlot = -2

    /** The chosen SIM's system switch as last read (null = not read yet), for the WebUI. */
    @Volatile var lastSwitch: Boolean? = null
        private set

    fun start() {
        val app = Core.app
        // Subscription database changes (the WLAN calling switch is a column of it).
        runCatching {
            app.contentResolver.registerContentObserver(Uri.parse("content://telephony/siminfo"), true,
                object : ContentObserver(Handler(Looper.getMainLooper())) {
                    override fun onChange(selfChange: Boolean) = request("siminfo", 1500, refresh = false)
                })
        }.onFailure { Core.appendLog("WARN", "无法监听系统 WLAN 通话开关（${it.message}），改为定时检查") }
        // SIM / eSIM profile changes.
        runCatching {
            app.getSystemService(SubscriptionManager::class.java).addOnSubscriptionsChangedListener(
                app.mainExecutor, object : SubscriptionManager.OnSubscriptionsChangedListener() {
                    override fun onSubscriptionsChanged() = request("subscriptions", 3000, refresh = true)
                })
        }
        // Safety net in case a change notification is missed.
        exec.scheduleWithFixedDelay({ if (ready) runCatching { evaluate() } }, 60, 60, TimeUnit.SECONDS)
        // At boot the app process starts before Telephony and root are fully up.
        exec.execute {
            waitForBoot()
            ready = true
            Core.refreshSimsBlocking()
            runCatching { evaluate() }.onFailure { Core.appendLog("WARN", "同步 WLAN 通话开关失败：${it.message}") }
        }
    }

    /** Re-checks the switch soon (debounced). [refresh] re-reads the SIM list first. */
    fun request(reason: String, delayMs: Long = 500, refresh: Boolean = false) {
        if (!ready) return
        synchronized(this) {
            pending?.cancel(false)
            pending = exec.schedule({
                if (refresh) Core.refreshSimsBlocking()
                runCatching { evaluate() }.onFailure { Core.appendLog("WARN", "同步 WLAN 通话开关失败（$reason）：${it.message}") }
            }, delayMs, TimeUnit.MILLISECONDS)
        }
    }

    /** WebUI: turn WiFi calling on/off by flipping the SIM's system switch. */
    fun setSwitch(on: Boolean) {
        exec.execute {
            val s = Core.settings
            if (on) s.paused = false
            val sub = s.subId
            if (sub <= 0) return@execute
            runCatching { Core.engine.setWifiCallingSetting(sub.toLong(), on) }
                .onSuccess { Core.appendLog("INFO", "已${if (on) "打开" else "关闭"}系统 WLAN 通话开关") }
                .onFailure { Core.appendLog("WARN", "设置系统 WLAN 通话开关失败：${it.message}") }
            runCatching { evaluate() }
        }
    }

    /** Pauses the module (restore native): the switch no longer drives it until turned on from the WebUI. */
    fun pause() {
        Core.settings.paused = true
        request("pause", 0)
    }

    private fun waitForBoot() {
        while (prop("sys.boot_completed") != "1") Thread.sleep(3000)
        // Telephony binds IMS services and loads SIMs shortly after boot.
        if (android.os.SystemClock.elapsedRealtime() < 5 * 60_000) Thread.sleep(15_000)
        waitForNetProxy()
    }

    /**
     * "Follow system" goes through NetProxy (the transparent proxy this module
     * supports) when it is installed. It starts after us at boot; connecting
     * first goes out directly, which carriers such as EE reject from China
     * (and then back off for a minute). Wait for it to report ready.
     */
    private fun waitForNetProxy() {
        if (Core.settings.networkMode != "auto") return
        if (android.os.SystemClock.elapsedRealtime() > 5 * 60_000) return
        val installed = root("[ -d /data/adb/modules/netproxy ] && [ ! -e /data/adb/modules/netproxy/disable ] && echo yes").trim() == "yes"
        if (!installed) return
        val deadline = System.currentTimeMillis() + 90_000
        while (System.currentTimeMillis() < deadline) {
            if (root("cat /dev/netproxy/service.json 2>/dev/null").contains("\"state\":\"ready\"")) {
                Core.appendLog("INFO", "NetProxy 已就绪，开始连接")
                return
            }
            Thread.sleep(3000)
        }
        Core.appendLog("WARN", "等待 NetProxy 就绪超时（90 秒），直接连接")
    }

    private fun prop(name: String): String = runCatching {
        Class.forName("android.os.SystemProperties").getMethod("get", String::class.java).invoke(null, name) as String
    }.getOrDefault("")

    @Synchronized
    private fun evaluate() {
        val s = Core.settings
        val sub = s.subId
        val slot = s.simSlot
        // The chosen SIM is missing from the list while an eSIM profile switch or a
        // SIM reload is in progress: wait for the next change instead of acting on it.
        if (!s.paused && sub > 0 && Core.sims.value.none { it.subId == sub }) return
        val switchOn = sub > 0 && Core.engine.wifiCallingSetting(sub.toLong())
        lastSwitch = switchOn
        val want = !s.paused && sub > 0 && slot >= 0 && switchOn
        val owned = (0..1).filter { takenOver(it) }

        // Slots that are ours but not wanted any more (switch off, or another SIM chosen).
        val release = owned.filter { !want || it != slot }
        if (release.isNotEmpty() || (!want && (s.enabled || Core.status.value.phase != "idle"))) {
            stopEngine()
            if (release.isNotEmpty()) {
                Core.restoreNative()
                release.forEach { root("cmd phone ims clear-ims-service-override -s $it") }
                Core.appendLog("INFO", "已把卡槽 ${release.joinToString { (it + 1).toString() }} 交还系统 IMS（恢复原生通话）")
            }
        }
        if (want) {
            if (slot !in owned) {
                root("cmd phone ims set-ims-service -s $slot -c -f 0,1 app.vowifi")
                Core.appendLog("INFO", "系统 WLAN 通话已开启：卡槽 ${slot + 1} 改由本模块的 IMS 服务接管")
            }
            if (!s.enabled || Core.status.value.phase == "idle") VoWiFiService.start(Core.app)
        }
        val now = if (want) slot else -1
        if (now != ownedSlot) {
            ownedSlot = now
            // Kept for uninstall.sh / the WebUI: which slot is ours right now.
            root("mkdir -p /data/adb/vowifi && echo 'SLOTS=\"${if (now >= 0) now else ""}\"' > /data/adb/vowifi/ims.conf")
        }
    }

    private fun stopEngine() {
        if (Core.status.value.phase == "idle" && !Core.settings.enabled) return
        VoWiFiService.stop(Core.app)
        val deadline = System.currentTimeMillis() + 15_000
        while (Core.status.value.phase != "idle" && System.currentTimeMillis() < deadline) Thread.sleep(300)
        Core.settings.enabled = false
        Core.appendLog("INFO", "系统 WLAN 通话已关闭：模块的 WiFi 通话已停止")
    }

    private fun takenOver(slot: Int): Boolean =
        root("cmd phone ims get-ims-service -s $slot -c").trim() == "app.vowifi"

    private fun root(cmd: String): String = runCatching {
        val p = ProcessBuilder("/system/bin/su", "-c", cmd).redirectErrorStream(true).start()
        val out = p.inputStream.bufferedReader().readText()
        p.waitFor(20, TimeUnit.SECONDS)
        out
    }.getOrDefault("")
}
