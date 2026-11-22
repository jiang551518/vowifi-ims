package app.vowifi.data

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject

/** Persistent user settings (SharedPreferences). */
class Settings(context: Context) {
    private val p = context.getSharedPreferences("settings", Context.MODE_PRIVATE)

    var subId: Int
        get() = p.getInt("subId", -1)
        set(v) = p.edit().putInt("subId", v).apply()

    /**
     * Physical slot of the chosen SIM. Switching profiles on an eSIM card gives
     * the slot a new subId, so the slot is what identifies "the user's choice".
     */
    var simSlot: Int
        get() = p.getInt("simSlot", -1)
        set(v) = p.edit().putInt("simSlot", v).apply()

    /** Turn the chosen SIM's cellular radio off while WiFi calling is on. */
    var radioOffWhileOn: Boolean
        get() = p.getBoolean("radioOffWhileOn", false)
        set(v) = p.edit().putBoolean("radioOffWhileOn", v).apply()

    /** Slot / subId whose radio the app turned off (-1 = none), to restore it later. */
    var radioOffSlot: Int
        get() = p.getInt("radioOffSlot", -1)
        set(v) = p.edit().putInt("radioOffSlot", v).apply()
    /** subIds whose operator brand override an earlier build set (cleared at start-up). */
    var brandSubs: Set<String>
        get() = p.getStringSet("brandSubs", emptySet()) ?: emptySet()
        set(v) = p.edit().putStringSet("brandSubs", v).apply()
    var radioOffMask: Long
        get() = p.getLong("radioOffMask", 0L)
        set(v) = p.edit().putLong("radioOffMask", v).apply()
    var radioOffSub: Int
        get() = p.getInt("radioOffSub", -1)
        set(v) = p.edit().putInt("radioOffSub", v).apply()

    /** auto = follow the system default network (VPN/proxy apps included); wifi; cellular. */
    var networkMode: String
        get() = p.getString("networkMode", "auto")!!
        set(v) = p.edit().putString("networkMode", v).apply()

    var socks5: String
        get() = p.getString("socks5", "")!!
        set(v) = p.edit().putString("socks5", v.trim()).apply()
    var socksUser: String
        get() = p.getString("socksUser", "")!!
        set(v) = p.edit().putString("socksUser", v).apply()
    var socksPass: String
        get() = p.getString("socksPass", "")!!
        set(v) = p.edit().putString("socksPass", v).apply()

    /** system | doh | static */
    var dnsMode: String
        get() = p.getString("dnsMode", "system")!!
        set(v) = p.edit().putString("dnsMode", v).apply()
    var dohServers: String
        get() = p.getString("doh", "https://223.5.5.5/dns-query, https://1.1.1.1/dns-query")!!
        set(v) = p.edit().putString("doh", v).apply()
    var staticEpdgIp: String
        get() = p.getString("staticIp", "")!!
        set(v) = p.edit().putString("staticIp", v.trim()).apply()

    var autoStart: Boolean
        get() = p.getBoolean("autoStart", false)
        set(v) = p.edit().putBoolean("autoStart", v).apply()
    /** Turn the SIM's "HD calling" (VoLTE) off while the module serves it, back on after. */
    var syncVolte: Boolean
        get() = p.getBoolean("syncVolte", false)
        set(v) = p.edit().putBoolean("syncVolte", v).apply()
    /** subIds whose "HD calling" this app turned off and must turn back on. */
    var volteRestoreSubs: Set<Int>
        get() {
            val set = p.getStringSet("volteRestoreSubs", emptySet())!!.mapNotNull { it.toIntOrNull() }.toMutableSet()
            p.getInt("volteRestoreSub", -1).takeIf { it > 0 }?.let { set += it } // older single-value record
            return set
        }
        set(v) = p.edit().putStringSet("volteRestoreSubs", v.map { it.toString() }.toSet()).remove("volteRestoreSub").apply()
    /** "Restore native calling" from the WebUI: the system switch no longer drives the module. */
    var paused: Boolean
        get() = p.getBoolean("paused", false)
        set(v) = p.edit().putBoolean("paused", v).apply()
    var enabled: Boolean
        get() = p.getBoolean("enabled", false)
        set(v) = p.edit().putBoolean("enabled", v).apply()
    var debugLog: Boolean
        get() = p.getBoolean("debugLog", false)
        set(v) = p.edit().putBoolean("debugLog", v).apply()
    var preferIpv6: Boolean
        get() = p.getBoolean("preferIpv6", false)
        set(v) = p.edit().putBoolean("preferIpv6", v).apply()

    // Carrier overrides (empty = use the built-in profile).
    var epdg: String
        get() = p.getString("epdg", "")!!
        set(v) = p.edit().putString("epdg", v.trim()).apply()
    var dialScheme: String
        get() = p.getString("dialScheme", "")!!
        set(v) = p.edit().putString("dialScheme", v).apply()
    var countryCode: String
        get() = p.getString("countryCode", "")!!
        set(v) = p.edit().putString("countryCode", v.trim()).apply()
    var smsc: String
        get() = p.getString("smsc", "")!!
        set(v) = p.edit().putString("smsc", v.trim()).apply()
    var imsDomain: String
        get() = p.getString("imsDomain", "")!!
        set(v) = p.edit().putString("imsDomain", v.trim()).apply()
    var pcscf: String
        get() = p.getString("pcscf", "")!!
        set(v) = p.edit().putString("pcscf", v.trim()).apply()
    /** "" = carrier default, "on", "off" */
    var secAgree: String
        get() = p.getString("secAgree", "")!!
        set(v) = p.edit().putString("secAgree", v).apply()
    var sipTransport: String
        get() = p.getString("sipTransport", "")!!
        set(v) = p.edit().putString("sipTransport", v).apply()

    /** Engine configuration JSON (see core/mobile.Config). */
    /** Per-install UUID (formerly the +sip.instance; no longer sent, see engineConfig). */
    val instanceId: String
        get() = p.getString("instanceId", null) ?: java.util.UUID.randomUUID().toString().also {
            p.edit().putString("instanceId", it).apply()
        }

    fun engineConfig(networkHandle: Long): String {
        val o = JSONObject()
        o.put("subId", subId)
        // No instanceId: the IMEI-based +sip.instance is required for calls on
        // EE-based SIMs (a UUID instance registers but every call gets 487).
        if (networkMode != "auto" && networkHandle != 0L) {
            o.put("network", "bound")
            o.put("networkHandle", networkHandle)
        } else o.put("network", "auto")
        o.put("socks5", socks5)
        o.put("socksUser", socksUser)
        o.put("socksPass", socksPass)
        o.put("dns", dnsMode)
        o.put("doh", JSONArray(dohServers.split(",").map { it.trim() }.filter { it.isNotEmpty() }))
        o.put("staticIp", staticEpdgIp)
        o.put("preferIpv6", preferIpv6)
        o.put("epdg", epdg)
        o.put("dialScheme", dialScheme)
        o.put("countryCode", countryCode)
        o.put("smsc", smsc)
        o.put("imsDomain", imsDomain)
        if (pcscf.isNotBlank()) o.put("pcscf", JSONArray(pcscf.split(",").map { it.trim() }.filter { it.isNotEmpty() }))
        when (secAgree) { "on" -> o.put("secAgree", true); "off" -> o.put("secAgree", false) }
        when (sipTransport) { "tcp" -> o.put("preferTcp", true); "udp" -> o.put("preferTcp", false) }
        return o.toString()
    }
}
