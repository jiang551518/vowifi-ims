package app.vowifi.service

import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.PowerManager
import androidx.lifecycle.LifecycleService
import androidx.lifecycle.lifecycleScope
import app.vowifi.Core
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Keeps the SWu tunnel and IMS registration alive. Network selection:
 *  - auto: follow the default network (includes VPN / proxy apps), reconnect on change
 *  - wifi / cellular: request that transport and bind the tunnel to it
 */
class VoWiFiService : LifecycleService() {

    private lateinit var cm: ConnectivityManager
    private var callback: ConnectivityManager.NetworkCallback? = null
    private var current: Network? = null
    private var pendingKick: Job? = null
    private var wakeLock: PowerManager.WakeLock? = null

    private var subsListener: android.telephony.SubscriptionManager.OnSubscriptionsChangedListener? = null
    private var subsJob: Job? = null

    override fun onCreate() {
        super.onCreate()
        // A profile switch on an eSIM card (or a card swap) replaces the slot's
        // subscription; re-read the SIMs so Core can follow the chosen slot.
        val sm = getSystemService(android.telephony.SubscriptionManager::class.java)
        subsListener = object : android.telephony.SubscriptionManager.OnSubscriptionsChangedListener() {
            override fun onSubscriptionsChanged() {
                subsJob?.cancel()
                subsJob = lifecycleScope.launch { delay(3000); Core.refreshSims() }
            }
        }.also { runCatching { sm.addOnSubscriptionsChangedListener(mainExecutor, it) } }
        cm = getSystemService(ConnectivityManager::class.java)
        Notifications.ensureChannels(this)
        startForeground(Notifications.SERVICE_ID, Notifications.service(this, Core.status.value),
            ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        lifecycleScope.launch {
            var lastPhase = ""
            Core.status.collect { st ->
                Notifications.updateService(this@VoWiFiService, st)
                // A proxy started after us adds its rules later: re-apply once per failure.
                if (st.phase == "waiting" && lastPhase != "waiting" && Core.settings.networkMode != "auto") {
                    syncBypass(force = true)
                }
                lastPhase = st.phase
            }
        }
        lifecycleScope.launch {
            Core.calls.collect { calls ->
                val active = calls.values.any { it.state == "active" || it.state == "ringing" || it.state == "dialing" }
                val type = if (active) ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE or ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
                else ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE
                runCatching { startForeground(Notifications.SERVICE_ID, Notifications.service(this@VoWiFiService, Core.status.value), type) }
            }
        }
        // A partial wake lock keeps IKE/SIP keepalives running in Doze.
        wakeLock = getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "vowifi:ims").apply { setReferenceCounted(false); acquire() }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        super.onStartCommand(intent, flags, startId)
        when (intent?.action) {
            ACTION_STOP -> {
                Core.settings.enabled = false
                Core.engine.stop()
                bypassMode = null
                Thread { ProxyBypass.apply(false); Core.syncCellularRadio(false) }.start()
                stopSelf()
                return START_NOT_STICKY
            }
            else -> {
                Core.settings.enabled = true
                // Re-read the SIMs first so a switched eSIM profile is picked up
                // before connecting (Core follows the chosen slot).
                Core.refreshSims {
                    lifecycleScope.launch {
                        syncBypass(force = true)
                        withContext(Dispatchers.IO) { Core.syncCellularRadio(true) }
                        setupNetwork()
                    }
                }
            }
        }
        return START_STICKY
    }

    /** Mode the proxy bypass was last applied for (null = not applied). */
    private var bypassMode: String? = null

    /**
     * WiFi / mobile-data modes must not be captured by a root transparent
     * proxy; "follow system" keeps going through it. Re-applied after a
     * failed attempt in case the proxy was (re)started meanwhile.
     */
    private suspend fun syncBypass(force: Boolean) {
        val mode = Core.settings.networkMode
        if (!force && mode == bypassMode) return
        bypassMode = mode
        val msg = withContext(Dispatchers.IO) { ProxyBypass.apply(mode != "auto") }
        if (mode != "auto" || force) Core.appendLog("INFO", msg)
    }

    private fun setupNetwork() {
        callback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        current = null
        val mode = Core.settings.networkMode
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                val changed = current != null && current != network
                val first = current == null
                current = network
                if (mode == "auto") {
                    if (first) startEngine(0) else if (changed) kick()
                } else {
                    startEngine(network.networkHandle)
                }
            }

            override fun onLost(network: Network) {
                if (network == current) {
                    current = null
                    Core.appendLog("WARN", "network lost")
                }
            }
        }
        callback = cb
        when (mode) {
            "wifi", "cellular" -> {
                val t = if (mode == "wifi") NetworkCapabilities.TRANSPORT_WIFI else NetworkCapabilities.TRANSPORT_CELLULAR
                val req = NetworkRequest.Builder()
                    .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                    .addTransportType(t).build()
                // requestNetwork keeps mobile data up even while Wi-Fi is connected.
                cm.requestNetwork(req, cb)
            }
            else -> cm.registerDefaultNetworkCallback(cb)
        }
    }

    private fun startEngine(handle: Long) {
        lifecycleScope.launch(Dispatchers.IO) {
            if (Core.settings.subId == -1) {
                Core.appendLog("ERROR", "no SIM selected")
                return@launch
            }
            Core.engine.setDebug(Core.settings.debugLog)
            runCatching { Core.engine.start(Core.settings.engineConfig(handle)) }
                .onFailure { Core.appendLog("ERROR", "start: ${it.message}") }
        }
    }

    /** Debounced reconnect after a default-network switch. */
    private fun kick() {
        pendingKick?.cancel()
        pendingKick = lifecycleScope.launch {
            delay(1500)
            Core.appendLog("INFO", "network changed, reconnecting")
            Core.engine.networkChanged()
        }
    }

    override fun onDestroy() {
        // Never leave the SIM without cellular service once WiFi calling stops.
        Thread { Core.syncCellularRadio(false) }.start()
        if (bypassMode != null && bypassMode != "auto") Thread { ProxyBypass.apply(false) }.start()
        subsListener?.let { l -> runCatching { getSystemService(android.telephony.SubscriptionManager::class.java).removeOnSubscriptionsChangedListener(l) } }
        callback?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        runCatching { wakeLock?.release() }
        Core.engine.stop()
        super.onDestroy()
    }

    companion object {
        const val ACTION_START = "app.vowifi.START"
        const val ACTION_STOP = "app.vowifi.STOP"

        fun start(ctx: Context) {
            ctx.startForegroundService(Intent(ctx, VoWiFiService::class.java).setAction(ACTION_START))
        }

        fun stop(ctx: Context) {
            ctx.startService(Intent(ctx, VoWiFiService::class.java).setAction(ACTION_STOP))
        }
    }
}
