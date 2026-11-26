package app.vowifi

import android.app.Application
import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import app.vowifi.call.CallAudio
import app.vowifi.control.WebBridge
import app.vowifi.core.mobile.Engine
import app.vowifi.core.mobile.Listener
import app.vowifi.core.mobile.Mobile
import app.vowifi.core.mobile.Resolver
import app.vowifi.data.SysLogCapture
import app.vowifi.ims.ImsHub
import app.vowifi.data.LogStore
import app.vowifi.data.Settings
import app.vowifi.service.Notifications
import app.vowifi.service.VoWiFiService
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.net.InetAddress
import java.time.Instant

data class EngineStatus(
    val phase: String = "idle",
    val message: String = "",
    val error: String = "",
    val attempt: Int = 0,
    val carrier: String = "",
    val epdg: String = "",
    val epdgIp: String = "",
    val innerIpv4: String = "",
    val innerIpv6: String = "",
    val pcscf: List<String> = emptyList(),
    val ike: String = "",
    val esp: String = "",
    val registered: Boolean = false,
    val number: String = "",
    val uris: List<String> = emptyList(),
    val security: String = "",
    val transport: String = "",
    val since: Long = 0,
    val nextRetry: Long = 0,
    val step: String = "",
    val failedStep: String = "",
    val plmn: String = "",
)

data class CallInfo(
    val id: String,
    val number: String,
    val incoming: Boolean,
    val state: String, // dialing ringing incoming active ended
    val codec: String = "",
    val maxMode: Int = 0,
    val octetAlign: Boolean = false,
    val earlyMedia: Boolean = false,
    val reason: String = "",
    val sipCode: Int = 0,
    val started: Long = 0,
    val answered: Long = 0,
    val rxPackets: Long = 0,
    val txPackets: Long = 0,
)

data class LogLine(val time: Long, val level: String, val msg: String)

data class SimInfo(
    val subId: Int, val slot: Int, val mcc: String, val mnc: String,
    val carrierName: String, val displayName: String, val iccid: String,
    val profileName: String, val verified: Boolean, val epdg: String,
)

/** Process-wide singleton wiring the Go engine to Android. */
object Core {
    lateinit var app: Application
        private set
    lateinit var settings: Settings
        private set
    lateinit var logStore: LogStore
        private set
    /** Errors in background coroutines are logged instead of crashing the (persistent) process. */
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default + kotlinx.coroutines.CoroutineExceptionHandler { ctx, e ->
        appendLog("ERROR", "后台任务异常（${ctx[kotlinx.coroutines.CoroutineName]?.name ?: "协程"}）：${android.util.Log.getStackTraceString(e)}")
    })

    private val _status = MutableStateFlow(EngineStatus())
    val status: StateFlow<EngineStatus> = _status
    private val _calls = MutableStateFlow<Map<String, CallInfo>>(emptyMap())
    val calls: StateFlow<Map<String, CallInfo>> = _calls
    private val _logs = MutableStateFlow<List<LogLine>>(emptyList())
    val logs: StateFlow<List<LogLine>> = _logs
    private val _sims = MutableStateFlow<List<SimInfo>>(emptyList())
    val sims: StateFlow<List<SimInfo>> = _sims
    private val _rootError = MutableStateFlow<String?>(null)
    val rootError: StateFlow<String?> = _rootError

    val engine: Engine by lazy {
        // Hand the Java environment to Go first: the root helper (app_process)
        // needs BOOTCLASSPATH and Go cannot always read /proc/self/environ.
        System.getenv().forEach { (k, v) -> Mobile.setEnv(k, v) }
        // Go panics go to files/logs/go_crash.log (Android drops native stderr);
        // a crash from the previous run is copied into the app log.
        runCatching {
            val crash = java.io.File(logStore.directory, "go_crash.log")
            if (crash.length() > 0) {
                val text = crash.readText()
                appendLog("ERROR", "上次引擎崩溃：" + text.lines().filter { it.isNotBlank() }.take(40).joinToString(" | "))
                crash.renameTo(java.io.File(logStore.directory, "go_crash.prev.log"))
            }
            Mobile.setCrashLog(crash.absolutePath)
        }
        Mobile.newEngine(app.applicationInfo.sourceDir, listener).also {
            it.setResolver(resolver)
            it.setDebug(settings.debugLog)
        }
    }

    fun init(application: Application) {
        app = application
        settings = Settings(application)
        logStore = LogStore(application)
        _logs.value = logStore.load(MAX_LOG_LINES)
        // Global capture: crashes go to the app log synchronously, and a root
        // logcat keeps IMS/telephony system logs in files/logs/system.log.
        val prevHandler = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { t, e ->
            logStore.appendNow(LogLine(System.currentTimeMillis(), "ERROR",
                "崩溃（线程 ${t.name}）：${android.util.Log.getStackTraceString(e)}"))
            prevHandler?.uncaughtException(t, e)
        }
        runCatching {
            val pi = application.packageManager.getPackageInfo(application.packageName, 0)
            appendLog("INFO", "App 启动：v${pi.versionName}（进程 ${android.os.Process.myPid()}）")
        }
        Thread {
            val r = SysLogCapture.ensureRunning(logStore.directory)
            if (r != "running") appendLog(if (r == "started") "INFO" else "WARN", "系统日志捕获：$r")
            Thread.sleep(15_000)
            clearBrandOverrides()
        }.start()
        ImsHub.setBackend(imsBackend)
        WebBridge.start()
        // The SIM's system "WLAN calling" switch takes the slot over / hands it back.
        WfcSync.start()
        // The registration technology follows the exit network (WiFi vs mobile data).
        runCatching {
            app.getSystemService(ConnectivityManager::class.java).registerDefaultNetworkCallback(
                object : ConnectivityManager.NetworkCallback() {
                    override fun onCapabilitiesChanged(network: android.net.Network, caps: NetworkCapabilities) = pushImsRegistration()
                    override fun onLost(network: android.net.Network) = pushImsRegistration()
                })
        }
    }

    /**
     * Whether the tunnel runs over mobile data rather than WiFi. Over mobile data
     * the registration is reported as cross-SIM (IMS over another SIM's data):
     * ColorOS hides the WiFi calling icon without WiFi unless it is cross-SIM.
     */
    private fun imsOverCellular(): Boolean = when (settings.networkMode) {
        "wifi" -> false
        "cellular" -> true
        else -> runCatching {
            val cm = app.getSystemService(ConnectivityManager::class.java)
            val caps = cm.getNetworkCapabilities(cm.activeNetwork)
            caps != null && !caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) &&
                caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)
        }.getOrDefault(false)
    }

    /** Registration state as reported to the system (lags behind during a quick reconnect). */
    @Volatile private var reportedRegistered = false
    private var deregJob: kotlinx.coroutines.Job? = null
    /** How long a reconnect may take before the system is told the SIM lost WiFi calling. */
    private const val DEREGISTER_GRACE_MS = 20_000L

    /**
     * Reports the IMS registration to the system. A tunnel rebuilt after a
     * network switch usually registers again within seconds; telling the
     * system right away makes the status bar flash "no service", so a loss
     * while the engine is reconnecting is only reported after a grace period.
     */
    private val regLock = Any()

    fun pushImsRegistration() = synchronized(regLock) {
        if (!::settings.isInitialized) return@synchronized
        val st = _status.value
        if (st.registered) {
            deregJob?.cancel()
            reportRegistration(true)
            return@synchronized
        }
        if (st.phase != "idle" && reportedRegistered) {
            if (deregJob?.isActive != true) deregJob = scope.launch {
                kotlinx.coroutines.delay(DEREGISTER_GRACE_MS)
                if (!_status.value.registered) {
                    appendLog("INFO", "重连超过 ${DEREGISTER_GRACE_MS / 1000} 秒，向系统报告 WiFi 通话暂不可用")
                    reportRegistration(false)
                }
            }
            return@synchronized
        }
        deregJob?.cancel()
        reportRegistration(false)
    }

    private fun reportRegistration(reg: Boolean) = synchronized(regLock) {
        reportedRegistered = reg
        ImsHub.onRegistration(settings.subId, reg, reg && imsOverCellular())
        syncVoiceOverride()
    }

    /**
     * Clears operator brand overrides an earlier build set. The carrier name
     * now always comes from the SIM / network as the system resolves it; a SIM
     * without a name of its own just shows none.
     */
    private fun clearBrandOverrides() {
        val subs = settings.brandSubs
        if (subs.isEmpty()) return
        voiceOverrideExec.execute {
            for (key in subs) {
                val sub = key.toIntOrNull() ?: continue
                runCatching { engine.setOperatorBrand(sub.toLong(), "") }
                    .onSuccess { settings.brandSubs = settings.brandSubs - key; appendLog("INFO", "已清除旧版设置的系统运营商名（subId $sub）") }
                    .onFailure { appendLog("WARN", "清除系统运营商名失败：${it.message}") }
            }
        }
    }

    private val crossSimDone = java.util.concurrent.ConcurrentHashMap.newKeySet<Int>()

    /**
     * Undoes every system change the app makes, so the phone behaves as before
     * (used before handing the slots back / uninstalling the module). Blocking;
     * call off the main thread with WiFi calling already stopped.
     */
    fun restoreNative() {
        // 2G restriction -> the SIM's previous network types.
        syncCellularRadio(false)
        // Cross-SIM calling: this platform offers no UI for it, so any SIM that
        // has it on got it from this app.
        _sims.value.forEach { s ->
            runCatching { engine.setCrossSimCalling(s.subId.toLong(), false) }
                .onSuccess { was -> if (was) appendLog("INFO", "已关闭 ${s.carrierName.ifEmpty { "subId ${s.subId}" }} 的跨卡通话设置") }
        }
        crossSimDone.clear()
        // "Voice in service over WiFi" overrides.
        val subs = voiceOverride.keys.toList() + listOfNotNull(settings.subId.takeIf { it > 0 })
        subs.distinct().forEach { s -> runCatching { engine.setVoiceServiceOverride(s.toLong(), false) } }
        voiceOverride.clear()
        clearBrandOverrides()
        appendLog("INFO", "已恢复手机原生通话设置")
    }

    /** Turns on the SIM's cross-SIM calling setting (needed for the status bar icon over mobile data). */
    private fun ensureCrossSimSetting(subId: Int) {
        if (subId <= 0 || !crossSimDone.add(subId)) return
        voiceOverrideExec.execute {
            runCatching { engine.setCrossSimCalling(subId.toLong(), true) }
                .onSuccess { before -> if (!before) appendLog("INFO", "已打开此卡的「跨卡通话」设置（无 WiFi 时状态栏显示 WiFi 通话图标需要）") }
                .onFailure { crossSimDone.remove(subId); appendLog("WARN", "打开跨卡通话设置失败：${it.message}") }
            pushImsRegistration()
        }
    }

    /** System IMS service (ImsService) -> engine. */
    /** subId -> override currently applied (true = voice "in service" over WiFi). */
    private val voiceOverride = java.util.concurrent.ConcurrentHashMap<Int, Boolean>()
    private val voiceOverrideExec = java.util.concurrent.Executors.newSingleThreadExecutor()

    /**
     * While our IMS registration serves the SIM the system IMS service runs
     * for, tell Telephony voice service is available (as native WiFi calling
     * does via IWLAN); otherwise the system dialer rejects calls with "cannot
     * connect to mobile network" when the SIM has no cellular coverage.
     */
    fun syncVoiceOverride() {
        val sub = settings.subId
        val want = sub > 0 && reportedRegistered && ImsHub.isActive(sub)
        val desired = HashMap<Int, Boolean>()
        voiceOverride.forEach { (s, on) -> if (on && s != sub) desired[s] = false }
        if (sub > 0 && voiceOverride[sub] != want && (want || voiceOverride[sub] == true || voiceOverride[sub] == null)) desired[sub] = want
        if (desired.isEmpty()) return
        desired.forEach { (s, on) -> voiceOverride[s] = on }
        voiceOverrideExec.execute {
            desired.forEach { (s, on) ->
                runCatching { engine.setVoiceServiceOverride(s.toLong(), on) }
                    .onSuccess { appendLog("INFO", if (on) "已向系统报告此卡可经 WiFi 通话（语音在服务）" else "已撤销此卡的 WiFi 语音在服务状态") }
                    .onFailure { voiceOverride.remove(s); appendLog("WARN", "设置系统语音服务状态失败：${it.message}") }
            }
        }
    }

    private val imsBackend = object : ImsHub.Backend {
        override fun onWifiCallingRequested(slot: Int, subId: Int, enabled: Boolean) {
            appendLog("INFO", "系统 WiFi 通话能力：卡槽 ${slot + 1} ${if (enabled) "开启" else "关闭"}")
            // Telephony only tells the bound IMS service. The SIM's system switch is
            // what decides (WfcSync); an eSIM profile switch also disables the old
            // subscription's capabilities first, which must not stop anything.
            if (enabled && slot == settings.simSlot && subId > 0) settings.subId = subId
            WfcSync.request("capabilities", 1000)
        }

        override fun onSubscriptionReady(slot: Int, subId: Int) {
            // The fastest sign of an eSIM profile switch: reconnect with the new
            // subscription right away instead of waiting for a retry or refresh.
            if (subId <= 0 || slot != settings.simSlot || subId == settings.subId || !settings.enabled) return
            appendLog("INFO", "卡槽 ${slot + 1} 已切换到新号码（subId $subId），立即用新号码连接 WiFi 通话")
            settings.subId = subId
            VoWiFiService.start(app)
        }

        override fun dial(subId: Int, number: String): String {
            appendLog("INFO", "系统拨号 $number")
            val o = JSONObject(engine.dial(number))
            handleCall(o)
            return o.getString("id")
        }

        override fun answer(callId: String) {
            runCatching { engine.answer(callId) }.onFailure { appendLog("ERROR", "answer: ${it.message}") }
        }

        override fun hangup(callId: String) {
            runCatching { engine.hangup(callId) }
        }

        override fun sendDtmf(callId: String, digit: Char) {
            runCatching { engine.sendDTMF(callId, digit.toString()) }
        }

        override fun setMute(mute: Boolean) = CallAudio.setMuted(mute)

        override fun sendSms(subId: Int, smsc: String?, tpdu: ByteArray): Int {
            val cause = engine.sendSMSPDU(smsc ?: "", tpdu).toInt()
            appendLog(if (cause == 0) "INFO" else "WARN",
                if (cause == 0) "系统短信已发送（短信中心已接收）" else "系统短信被短信中心拒收（RP cause $cause）")
            return cause
        }

        override fun ackSms(id: String, ok: Boolean, rpCause: Int) {
            runCatching { engine.ackSMS(id, ok, rpCause.toLong()) }
            if (!ok) appendLog("WARN", "系统短信 App 未接收短信（RP cause $rpCause）")
        }

        override fun setSmsTakeover(subId: Int, on: Boolean) {
            if (subId != settings.subId) return
            runCatching { engine.setRawSMS(on) }
            appendLog("INFO", if (on) "短信改由系统短信 App 收发" else "短信恢复由 App 收发")
            if (on) ensureCrossSimSetting(subId)
            syncVoiceOverride()
        }

        override fun log(level: String, msg: String) = appendLog(level, msg)
    }

    // ------------------------------------------------------------ engine IO

    private val listener = object : Listener {
        override fun onEvent(kind: String, json: String) {
            try {
                handleEvent(kind, json)
            } catch (t: Throwable) {
                appendLog("ERROR", "event $kind: ${t.message}")
            }
        }

        override fun onAudio(callID: String, frame: ByteArray) = CallAudio.onFrame(callID, frame)

        override fun onDTMF(callID: String, digit: String) = appendLog("INFO", "DTMF received: $digit")
    }

    val resolver = object : Resolver {
        override fun resolve(host: String, networkHandle: Long): String {
            val cm = app.getSystemService(ConnectivityManager::class.java)
            val net = if (networkHandle != 0L) cm.allNetworks.firstOrNull { it.networkHandle == networkHandle } else null
            val addrs: Array<InetAddress> = net?.getAllByName(host) ?: InetAddress.getAllByName(host)
            return addrs.joinToString(",") { it.hostAddress ?: "" }
        }
    }

    private fun time(s: String?): Long =
        if (s.isNullOrEmpty() || s.startsWith("0001")) 0 else runCatching { Instant.parse(s).toEpochMilli() }.getOrDefault(0)

    private fun strings(a: JSONArray?): List<String> = (0 until (a?.length() ?: 0)).map { a!!.getString(it) }

    private fun handleEvent(kind: String, json: String) {
        when (kind) {
            "status" -> {
                val o = JSONObject(json)
                val reg = o.optJSONObject("registration")
                _status.value = EngineStatus(
                    phase = o.optString("phase"), message = o.optString("message"), error = o.optString("error"),
                    attempt = o.optInt("attempt"), carrier = o.optString("carrier"), epdg = o.optString("epdg"),
                    epdgIp = o.optString("epdgIp"), innerIpv4 = o.optString("innerIpv4"), innerIpv6 = o.optString("innerIpv6"),
                    pcscf = strings(o.optJSONArray("pcscf")), ike = o.optString("ike"), esp = o.optString("esp"),
                    registered = reg?.optBoolean("registered") == true && o.optString("phase") == "registered",
                    number = reg?.optString("number") ?: "", uris = strings(reg?.optJSONArray("uris")),
                    security = reg?.optString("security") ?: "", transport = reg?.optString("transport") ?: "",
                    since = time(o.optString("since")), nextRetry = time(o.optString("nextRetry")),
                    step = o.optString("step"), failedStep = o.optString("failedStep"), plmn = o.optString("plmn"),
                )
                pushImsRegistration()
                syncVoiceOverride()
            }
            "registration" -> {
                val o = JSONObject(json)
                if (!o.optBoolean("registered")) appendLog("WARN", "IMS registration lost: ${o.optString("error")}")
            }
            "sms" -> {
                val o = JSONObject(json)
                val from = o.optString("from")
                val text = o.optString("text")
                // Only when the system SMS stack does not own this SIM (no
                // takeover): the app keeps no inbox, just shows the message.
                appendLog("INFO", "收到短信（系统短信未接管）")
                Notifications.showSms(app, from, text)
            }
            "sms_raw" -> {
                val o = JSONObject(json)
                val id = o.optString("id")
                val hex = o.optString("pdu")
                val pdu = ByteArray(hex.length / 2) { i -> hex.substring(2 * i, 2 * i + 2).toInt(16).toByte() }
                if (ImsHub.onSmsPdu(settings.subId, id, pdu, o.optBoolean("report"))) {
                    appendLog("INFO", if (o.optBoolean("report")) "短信回执已交给系统" else "收到短信，已交给系统短信 App")
                } else {
                    // Nobody to hand it to: temporary failure, the SMSC retries later.
                    runCatching { engine.ackSMS(id, false, 41) }
                    appendLog("WARN", "收到短信但系统短信通道未就绪，已请短信中心稍后重发")
                }
            }
            "call" -> handleCall(JSONObject(json))
            "log" -> {
                val o = JSONObject(json)
                appendLog(o.optString("level"), o.optString("msg"))
            }
        }
    }

    @Synchronized
    private fun handleCall(o: JSONObject) {
        val c = CallInfo(
            id = o.optString("id"), number = o.optString("number"), incoming = o.optBoolean("incoming"),
            state = o.optString("state"), codec = o.optString("codec"), maxMode = o.optInt("maxMode"),
            octetAlign = o.optBoolean("octetAlign"), earlyMedia = o.optBoolean("earlyMedia"),
            reason = o.optString("reason"), sipCode = o.optInt("sipCode"),
            started = time(o.optString("started")), answered = time(o.optString("answered")),
            rxPackets = o.optLong("rxPackets"), txPackets = o.optLong("txPackets"),
        )
        val prev = _calls.value[c.id]
        _calls.update { it + (c.id to c) }
        // Calls owned by the system dialer (ImsService takeover): Telephony rings,
        // shows the UI and logs the call; we only provide audio.
        if (c.state == "incoming" && prev == null && ImsHub.isActive(settings.subId) &&
            ImsHub.onIncomingCall(settings.subId, c.id, c.number)) {
            appendLog("INFO", "来电已交给系统电话：${c.number}")
            return
        }
        if (ImsHub.manages(c.id)) {
            // A failure reporting to Telephony must never keep the call silent.
            runCatching { ImsHub.onCallState(c.id, c.state, c.sipCode, c.reason, c.earlyMedia) }
                .onFailure { appendLog("ERROR", "通话状态交给系统失败（${c.state}）：$it") }
            when (c.state) {
                "ringing" -> if (c.earlyMedia && c.codec.isNotEmpty()) CallAudio.start(app, c)
                "active" -> CallAudio.start(app, c)
                "ended" -> {
                    CallAudio.stop(c.id)
                    appendLog("INFO", "RTP 收 ${c.rxPackets} 包，发 ${c.txPackets} 包")
                    appendLog("INFO", "call ended: ${c.reason} ${if (c.sipCode != 0) c.sipCode else ""}")
                    callFailHint(c.reason)?.let { hint ->
                        appendLog("WARN", "通话失败：$hint（网络原因 ${c.reason}）")
                        runCatching { Notifications.showCallHint(app, "通话失败：余额不足", hint) }
                    }
                    scope.launch { kotlinx.coroutines.delay(2500); _calls.update { it - c.id } }
                }
            }
            return
        }
        // Calls are handled by the system dialer only. Without the system IMS
        // takeover there is nothing to ring: decline so the caller is not left waiting.
        when (c.state) {
            "incoming" -> if (prev == null) {
                appendLog("WARN", "来电 ${c.number}：系统 IMS 未接管此卡，已拒接（请确认模块接管和系统 WLAN 通话开关）")
                scope.launch(Dispatchers.IO) { runCatching { engine.hangup(c.id) } }
            }
            "ended" -> {
                CallAudio.stop(c.id)
                scope.launch { kotlinx.coroutines.delay(2500); _calls.update { it - c.id } }
            }
        }
    }

    /**
     * Chinese explanation for call failures the user can act on, from the
     * network's Reason text (e.g. 487 with "CC_DIAM_CREDIT_LIMIT_REACHED": the
     * carrier's online charging (Diameter Credit-Control) refused the call).
     */
    fun callFailHint(reason: String?): String? {
        val r = reason?.uppercase() ?: return null
        return when {
            "CREDIT_LIMIT" in r || "INSUFFICIENT" in r || "OUT_OF_CREDIT" in r || "NO_CREDIT" in r ->
                "此卡余额不足（运营商计费系统拒绝了本次通话），请充值后再拨"
            else -> null
        }
    }

    fun appendLog(level: String, msg: String) {
        // Mirror to logcat (tag VoWiFi) so problems can be diagnosed over adb.
        when (level) {
            "ERROR" -> android.util.Log.e("VoWiFi", msg)
            "WARN" -> android.util.Log.w("VoWiFi", msg)
            "DEBUG" -> android.util.Log.d("VoWiFi", msg)
            else -> android.util.Log.i("VoWiFi", msg)
        }
        val line = LogLine(System.currentTimeMillis(), level, msg)
        val l = _logs.value
        _logs.value = (if (l.size > MAX_LOG_LINES) l.drop(1000) else l) + line
        if (::logStore.isInitialized) logStore.append(line)
    }

    /** Deletes the in-memory and on-disk log. */
    fun clearLogs() {
        _logs.value = emptyList()
        logStore.clear()
    }

    private const val MAX_LOG_LINES = 5000

    // ------------------------------------------------------------ actions

    private val radioLock = Any()
    /** Marker for the current blocking method (manual selection of 001-01). */
    private const val BLOCK_METHOD = 2L
    /** Allowed network types: 2G only, and Android's default for the power reason. */
    private const val TWO_G_ONLY = 32771L
    private const val DEFAULT_TYPES = 840583L

    /**
     * Applies the "cellular off while WiFi calling is on" option. [active] is
     * whether WiFi calling is running. Blocking (root call); call off the main
     * thread. Skipped in airplane mode, which already has every radio off.
     * Implemented by restricting that SIM to 2G: the modem's own VoLTE needs
     * LTE/NR, so it goes offline (see Engine.SetCellularBlocked for why
     * radio-off and a manual selection of a missing network are not used).
     */
    fun syncCellularRadio(active: Boolean) = synchronized(radioLock) {
        val airplane = android.provider.Settings.Global.getInt(app.contentResolver,
            android.provider.Settings.Global.AIRPLANE_MODE_ON, 0) == 1
        val want = active && settings.radioOffWhileOn && settings.subId != -1
        val offSlot = settings.radioOffSlot
        if (offSlot != -1 && (!want || offSlot != settings.simSlot)) {
            // The slot's subId changes when an eSIM profile is switched.
            val sub = _sims.value.firstOrNull { it.slot == offSlot }?.subId ?: settings.radioOffSub
            if (!airplane) runCatching { engine.setCellularBlocked(sub.toLong(), false, settings.radioOffMask) }
                .onSuccess { appendLog("INFO", "卡槽 ${offSlot + 1} 已恢复 4G/5G") }
                .onFailure { appendLog("WARN", "恢复卡槽 ${offSlot + 1} 蜂窝信号失败：${it.message}") }
            settings.radioOffSlot = -1
            settings.radioOffSub = -1
            settings.radioOffMask = 0L
        }
        // radioOffMask holds the allowed-types mask to restore; values <= 2
        // mean an older build blocked the slot another way: re-apply.
        if (want && (settings.radioOffSlot == -1 || settings.radioOffMask <= BLOCK_METHOD) && !airplane) {
            val slot = settings.simSlot
            // Record first: the slot must always be restored later.
            settings.radioOffSlot = slot
            settings.radioOffSub = settings.subId
            if (settings.radioOffMask <= BLOCK_METHOD) settings.radioOffMask = DEFAULT_TYPES
            runCatching { engine.setCellularBlocked(settings.subId.toLong(), true, 0L) }
                .onSuccess { before ->
                    if (before > BLOCK_METHOD && before != TWO_G_ONLY) settings.radioOffMask = before
                    appendLog("INFO", "卡槽 ${slot + 1} 已切换为仅 2G（停用 4G/5G，系统自带 VoLTE 下线；SIM 仍用于 WiFi 通话认证）")
                }
                .onFailure { appendLog("WARN", "切换 2G 失败：${it.message}") }
        }
    }

    fun refreshSims(onDone: (String?) -> Unit = {}) {
        scope.launch(Dispatchers.IO) { onDone(refreshSimsBlocking()) }
    }

    private val simLock = Any()

    /** Re-reads the SIM list; returns an error text or null. */
    fun refreshSimsBlocking(): String? = synchronized(simLock) {
            try {
                val arr = JSONArray(engine.subscriptionsJSON())
                _sims.value = (0 until arr.length()).map {
                    val o = arr.getJSONObject(it)
                    val p = o.optJSONObject("profile")
                    SimInfo(
                        o.optInt("subId"), o.optInt("slot"), o.optString("mcc"), o.optString("mnc"),
                        o.optString("carrierName"), o.optString("displayName"), o.optString("iccid"),
                        p?.optString("name") ?: "", p?.optBoolean("verified") == true, o.optString("epdg"),
                    )
                }
                _rootError.value = null
                if (settings.subId == -1) _sims.value.firstOrNull { it.mcc != "460" }?.let {
                    settings.subId = it.subId
                    settings.simSlot = it.slot
                }
                followSlot()
                null
            } catch (t: Throwable) {
                _rootError.value = t.message
                t.message ?: t.javaClass.simpleName
            }
    }

    /** Tears down and rebuilds the tunnel + IMS registration (user-requested restart). */
    fun restartWifiCalling(ctx: Context) {
        appendLog("INFO", "手动重启 WiFi 通话")
        VoWiFiService.start(ctx)
    }

    /**
     * Keeps WiFi calling on the chosen slot when its subscription changes (an
     * eSIM profile switch or a card swap gives the slot a new subId): adopts the
     * slot's new subscription and reconnects.
     */
    private fun followSlot() {
        val list = _sims.value
        val current = list.firstOrNull { it.subId == settings.subId }
        if (current != null) {
            if (settings.simSlot != current.slot) settings.simSlot = current.slot
            return
        }
        // Chosen subscription is gone (eSIM profile switch / card swap).
        val slot = settings.simSlot.takeIf { it >= 0 } ?: return
        val replacement = list.firstOrNull { it.slot == slot } ?: run {
            appendLog("WARN", "卡槽 ${slot + 1} 当前没有可用号码")
            return
        }
        settings.subId = replacement.subId
        settings.simSlot = slot
        appendLog("INFO", "卡槽 ${slot + 1} 的号码已更换为 ${replacement.displayName.ifEmpty { replacement.carrierName }}，WiFi 通话改用新号码")
        // Reconnect at once if WiFi calling is on; a starting service reads the new subId itself.
        if (_status.value.phase != "idle" || settings.enabled) VoWiFiService.start(app)
    }

    fun isConnected(ctx: Context): Boolean = status.value.registered
}
