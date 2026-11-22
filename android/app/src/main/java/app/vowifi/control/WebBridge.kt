package app.vowifi.control

import app.vowifi.Core
import app.vowifi.EngineStatus
import app.vowifi.SimInfo
import app.vowifi.WfcSync
import app.vowifi.ims.ImsHub
import app.vowifi.service.ExitIp
import app.vowifi.service.VoWiFiService
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.io.File

/**
 * Bridge to the module's KernelSU WebUI (module/webroot). The app has no UI of
 * its own: it writes its state to files/webui/status.json (read by the WebUI
 * through root) and takes commands from [ControlReceiver] broadcasts.
 */
object WebBridge {
    /** UI-only state that used to live in the Compose screens. */
    data class Extra(
        val exitIp: String = "",
        val exitChecking: Boolean = false,
        val radioBusy: String = "",
        val note: String = "",
        /** "running:<id>" / "done:<id>": progress of [restoreNative] for the WebUI's request id. */
        val restore: String = "",
        /** Cellular status of every SIM, by slot (see [CellText]). */
        val cell: JSONArray? = null,
    )

    val extra = MutableStateFlow(Extra())
    private val dirty = MutableStateFlow(0L)
    private val statusFile by lazy { File(File(Core.app.filesDir, "webui").apply { mkdirs() }, "status.json") }

    @OptIn(FlowPreview::class)
    fun start() {
        Core.scope.launch(Dispatchers.IO) {
            combine(Core.status, Core.sims, Core.calls, Core.rootError, extra) { _, _, _, _, _ -> System.nanoTime() }
                .combine(dirty) { a, _ -> a }
                .debounce(250)
                .collect { runCatching { write() } }
        }
    }

    /** Re-writes the status file (after a settings change, etc.). */
    fun touch() { dirty.value = System.nanoTime() }

    private fun write() {
        val st = Core.status.value
        val s = Core.settings
        val o = JSONObject()
            .put("updated", System.currentTimeMillis())
            .put("engine", engineJson(st))
            .put("sims", JSONArray().apply { Core.sims.value.forEach { put(simJson(it)) } })
            .put("rootError", Core.rootError.value ?: "")
            .put("inCall", Core.calls.value.values.any { it.state != "ended" })
            .put("ims", JSONObject()
                .put("active", s.subId > 0 && ImsHub.isActive(s.subId))
                .put("switch", WfcSync.lastSwitch ?: JSONObject.NULL)
                .put("paused", s.paused))
        val x = extra.value
        o.put("exitIp", x.exitIp).put("exitChecking", x.exitChecking).put("radioBusy", x.radioBusy).put("note", x.note)
            .put("restore", x.restore).put("cell", x.cell ?: JSONArray())
        o.put("settings", JSONObject()
            .put("subId", s.subId).put("simSlot", s.simSlot).put("enabled", s.enabled)
            .put("networkMode", s.networkMode).put("radioOffWhileOn", s.radioOffWhileOn)
            .put("syncVolte", s.syncVolte)
            .put("autoStart", s.autoStart).put("debugLog", s.debugLog).put("preferIpv6", s.preferIpv6)
            .put("socks5", s.socks5).put("socksUser", s.socksUser).put("socksPass", if (s.socksPass.isEmpty()) "" else "******")
            .put("dnsMode", s.dnsMode).put("dohServers", s.dohServers).put("staticEpdgIp", s.staticEpdgIp)
            .put("epdg", s.epdg).put("imsDomain", s.imsDomain).put("pcscf", s.pcscf)
            .put("countryCode", s.countryCode).put("smsc", s.smsc).put("dialScheme", s.dialScheme)
            .put("secAgree", s.secAgree).put("sipTransport", s.sipTransport))
        val tmp = File(statusFile.parentFile, "status.json.tmp")
        tmp.writeText(o.toString())
        tmp.renameTo(statusFile)
    }

    private fun engineJson(st: EngineStatus) = JSONObject()
        .put("phase", st.phase).put("step", st.step).put("failedStep", st.failedStep)
        .put("message", st.message).put("error", st.error).put("errorText", errorText(st.error))
        .put("attempt", st.attempt).put("carrier", st.carrier).put("plmn", st.plmn)
        .put("epdg", st.epdg).put("epdgIp", st.epdgIp).put("innerIpv4", st.innerIpv4).put("innerIpv6", st.innerIpv6)
        .put("pcscf", JSONArray(st.pcscf)).put("ike", st.ike).put("esp", st.esp)
        .put("security", st.security).put("transport", st.transport).put("uris", JSONArray(st.uris))
        .put("number", st.number).put("registered", st.registered)
        .put("since", st.since).put("nextRetry", st.nextRetry)

    private fun simJson(s: SimInfo) = JSONObject()
        .put("subId", s.subId).put("slot", s.slot).put("mcc", s.mcc).put("mnc", s.mnc)
        .put("name", operatorName(s)).put("displayName", s.displayName)
        .put("profileName", s.profileName).put("verified", s.verified)

    // ------------------------------------------------------------ commands

    /** Turns the chosen SIM's system WLAN calling switch on: the module takes the slot over and connects. */
    fun startWifiCalling(): String {
        if (Core.settings.subId == -1) return "请先选择 SIM 卡"
        WfcSync.setSwitch(true)
        return ""
    }

    /** Turns the system switch off: WiFi calling stops and the slot goes back to the phone's own IMS. */
    fun stopWifiCalling() = WfcSync.setSwitch(false)

    /**
     * Chooses the SIM the module serves. Its own system switch then decides; the
     * previous SIM's slot is handed back to the phone's own IMS.
     */
    fun selectSim(subId: Int) {
        val sim = Core.sims.value.firstOrNull { it.subId == subId } ?: return
        val changed = sim.subId != Core.settings.subId
        Core.settings.subId = sim.subId
        Core.settings.simSlot = sim.slot
        if (changed && Core.status.value.phase != "idle") VoWiFiService.start(Core.app)
        WfcSync.request("sim", 300)
        touch()
    }

    fun setNetworkMode(mode: String) {
        if (mode !in setOf("auto", "wifi", "cellular")) return
        Core.settings.networkMode = mode
        if (Core.status.value.phase != "idle") VoWiFiService.start(Core.app)
        touch()
        checkExitIp(delayMs = 800)
    }

    fun checkExitIp(delayMs: Long = 0) {
        if (extra.value.exitChecking) return
        extra.value = extra.value.copy(exitChecking = true)
        Core.scope.launch(Dispatchers.IO) {
            if (delayMs > 0) delay(delayMs) // let a mode change settle (bypass, network binding)
            val mode = Core.settings.networkMode
            val running = Core.status.value.phase != "idle"
            val text = runCatching { ExitIp.check(Core.app, mode, running) }
                .fold({ r -> listOf("${r.via} · ${r.ip}", listOf(r.flag, r.country).filter { it.isNotEmpty() }.joinToString(" "))
                    .filter { it.isNotBlank() }.joinToString(" · ") },
                    { e ->
                        Core.appendLog("WARN", "出口 IP 检测失败（$mode）：${e.message ?: e.javaClass.simpleName}")
                        "检测失败，详情见日志"
                    })
            extra.value = extra.value.copy(exitIp = text, exitChecking = false)
        }
    }

    /**
     * Read-only cellular status of every SIM for the WebUI (network type, band,
     * signal). The preferred network type itself is changed in
     * the system Settings only.
     */
    fun refreshCell() {
        val sims = Core.sims.value
        if (sims.isEmpty()) return
        Core.scope.launch(Dispatchers.IO) {
            val arr = JSONArray()
            for (sim in sims.sortedBy { it.slot }) {
                val o = runCatching { JSONObject(Core.engine.cellInfoJSON(sim.slot.toLong(), sim.subId.toLong())) }.getOrNull() ?: continue
                arr.put(CellText.format(o).put("slot", sim.slot).put("subId", sim.subId).put("name", operatorName(sim)))
            }
            extra.value = extra.value.copy(cell = arr)
        }
    }

    /**
     * "Switch this SIM to 2G while WiFi calling is on". Shows a busy state
     * until the SIM has really left LTE/NR (or selection is automatic again).
     */
    fun setRadioOff(on: Boolean) {
        if (extra.value.radioBusy.isNotEmpty()) return
        val running = Core.status.value.phase != "idle"
        Core.settings.radioOffWhileOn = on
        val slot = Core.settings.simSlot + 1
        val target = when {
            on && running -> 2
            !on && Core.settings.radioOffSlot != -1 -> 1
            else -> 0
        }
        extra.value = extra.value.copy(radioBusy = when (target) {
            2 -> "正在将卡槽 $slot 切换为 2G…"
            1 -> "正在恢复卡槽 $slot 的 4G/5G…"
            else -> ""
        })
        touch()
        Core.scope.launch(Dispatchers.IO) {
            try {
                Core.syncCellularRadio(running)
                if (target != 0) {
                    // Blocking: done once the SIM's data is off LTE/NR (RIL RAT 14 LTE,
                    // 19 LTE-CA, 20 NR) for two polls. Restoring: once selection is automatic.
                    val deadline = System.currentTimeMillis() + 60_000
                    var stable = 0
                    while (System.currentTimeMillis() < deadline) {
                        val f = runCatching { Core.engine.cellularState((slot - 1).toLong()) }
                            .getOrDefault("").split(",").map { it.trim() }
                        if (f.size >= 4) {
                            val onLte = f[3] in setOf("14", "19", "20")
                            val ok = if (target == 2) !onLte else f[2] != "true"
                            stable = if (ok) stable + 1 else 0
                            if (stable >= 2) break
                        }
                        Thread.sleep(2000)
                    }
                }
            } finally {
                extra.value = extra.value.copy(radioBusy = "")
            }
        }
    }

    /** Applies the settings page (JSON from the WebUI); reconnects if running. */
    fun saveSettings(o: JSONObject) {
        val s = Core.settings
        fun str(k: String, set: (String) -> Unit) { if (o.has(k)) set(o.optString(k).trim()) }
        str("socks5") { s.socks5 = it }
        str("socksUser") { s.socksUser = it }
        if (o.has("socksPass") && o.optString("socksPass") != "******") s.socksPass = o.optString("socksPass")
        str("dnsMode") { s.dnsMode = it }
        str("dohServers") { s.dohServers = it }
        str("staticEpdgIp") { s.staticEpdgIp = it }
        str("epdg") { s.epdg = it }
        str("imsDomain") { s.imsDomain = it }
        str("pcscf") { s.pcscf = it }
        str("countryCode") { s.countryCode = it }
        str("smsc") { s.smsc = it }
        str("dialScheme") { s.dialScheme = it }
        str("secAgree") { s.secAgree = it }
        str("sipTransport") { s.sipTransport = it }
        if (o.has("preferIpv6")) s.preferIpv6 = o.optBoolean("preferIpv6")
        Core.appendLog("INFO", "已保存设置")
        if (Core.status.value.phase != "idle") VoWiFiService.start(Core.app)
        touch()
    }

    /**
     * Stops WiFi calling and undoes the app's system changes (2G restriction,
     * cross-SIM setting, voice override). The WebUI then hands the IMS slots
     * back to the phone's own IMS and, if asked, removes the module.
     */
    fun restoreNative(id: String) {
        if (extra.value.restore.startsWith("running")) return
        extra.value = extra.value.copy(restore = "running:$id")
        Core.scope.launch(Dispatchers.IO) {
            try {
                Core.settings.autoStart = false
                // Paused: the system switch no longer drives the module (until it is
                // turned on from the WebUI again); hands the slot back.
                WfcSync.pause()
                if (Core.status.value.phase != "idle") {
                    VoWiFiService.stop(Core.app)
                    val deadline = System.currentTimeMillis() + 15_000
                    while (Core.status.value.phase != "idle" && System.currentTimeMillis() < deadline) delay(300)
                }
                Core.settings.enabled = false
                Core.restoreNative()
            } catch (t: Throwable) {
                Core.appendLog("ERROR", "恢复原生设置失败：${t.message}")
            } finally {
                extra.value = extra.value.copy(restore = "done:$id")
            }
        }
    }

    fun setAutoStart(on: Boolean) { Core.settings.autoStart = on; touch() }

    fun setDebugLog(on: Boolean) {
        Core.settings.debugLog = on
        runCatching { Core.engine.setDebug(on) }
        touch()
    }

    // ------------------------------------------------------------ texts

    /** Explains the most common engine errors in Chinese; keeps the raw text for the rest. */
    fun errorText(e: String): String = when {
        e.isEmpty() -> ""
        e.contains("not allowed in this location", true) ->
            "运营商拒绝注册：不允许在当前位置使用 WiFi 通话（运营商按网络出口判断你不在本国）"
        e.contains("Unable to resolve host") && e.contains("epdg", true) ->
            "找不到该运营商的 ePDG 地址，可在设置中手动填写 ePDG"
        e.contains("AUTHORIZATION_REJECTED") || e.contains("NON_3GPP_ACCESS_TO_EPC_NOT_ALLOWED") ->
            "运营商拒绝了本次 WiFi 通话接入授权（可能不允许从当前网络或地区接入）"
        e.contains("EAP authentication failed") -> "运营商拒绝了 SIM 认证（可能未开通 WiFi 通话）：$e"
        else -> e
    }

    /** Status texts Android puts in carrierName instead of an operator. */
    private val noServiceNames = setOf("无服务", "无信号", "仅限紧急呼叫", "紧急呼叫", "No service", "Emergency calls only", "")

    private val plmnNames = mapOf(
        "46000" to "中国移动", "46002" to "中国移动", "46004" to "中国移动", "46007" to "中国移动", "46008" to "中国移动",
        "46001" to "中国联通", "46006" to "中国联通", "46009" to "中国联通",
        "46003" to "中国电信", "46005" to "中国电信", "46011" to "中国电信",
        "46015" to "中国广电",
    )

    /**
     * Operator shown as the SIM title. carrierName is "Visited — SPN" while
     * roaming, the plain operator at home, and "无服务" etc. without service.
     */
    fun operatorName(s: SimInfo): String {
        val parts = s.carrierName.split("—").map { it.trim() }
        parts.getOrNull(1)?.takeIf { it.isNotEmpty() }?.let { return it }
        plmnNames[s.mcc + s.mnc]?.let { return it }
        // Foreign SIM: while roaming carrierName is just the visited network, prefer the profile's brand.
        val profile = s.profileName.substringBefore("（").trim()
        if (profile.isNotEmpty() && profile != "3GPP standard") return profile
        val name = parts.first()
        if (name !in noServiceNames && name !in plmnNames.values) return name
        return s.displayName.ifEmpty { "${s.mcc}-${s.mnc}" }
    }
}
